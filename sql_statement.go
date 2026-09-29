package forgeops

import (
	"errors"
	"strings"
)

// Finds the SQL behind a database error and reduces it to something safe to send: the names of the
// stored procedures, tables and views it touched, and (only if Configuration.CaptureSQLStatement
// is on) the statement itself with every string and number replaced by "?". Ported from
// gems/forge_ops_tracker's SqlStatement, which is itself ported from the server's own
// SqlStatementMasker/SqlObjectExtractor: same rules everywhere, and the server applies them again
// on arrival, so a difference here can only ever mean less is masked client-side, never that
// something unmasked gets stored.
//
// Written as a hand-rolled scanner and tokenizer rather than regular expressions: Go's RE2 engine
// deliberately has no lookbehind, lookahead or backreferences, which the shared pattern relies on
// (a number is only a value when it isn't part of an identifier, a string's E/X/N/B/U& prefix only
// when it isn't the end of a word, and a dollar-quoted body ends at the same tag that opened it).
// The rules are identical; only the mechanism differs. Strings allow '' and backslash escapes;
// numbers include hex, binary, exponents and leading-dot decimals; "double quoted" text is a
// string only for MySQL and MariaDB, and a name (left alone) everywhere else.
//
// Deliberately not a SQL parser.

const (
	sqlMask       = "?"
	maxSQLLength  = 4000
	maxSQLNames   = 10
	maxNameLength = 200
	maxCauseDepth = 5
)

// SQLObjects is what SQLStatement extraction found: the operation (the statement's first keyword),
// the stored procedures/functions it called, and the tables/views it touched. A view and a table
// are written the same way in SQL text, so both land in Relations.
type SQLObjects struct {
	Operation  string   `json:"operation,omitempty"`
	Procedures []string `json:"procedures"`
	Relations  []string `json:"relations"`
}

// A Go error carries no statement of its own, and no Go database driver puts one on its error
// types, so the SQL has to be attached explicitly by the code that ran the query. WithSQL wraps
// err with the statement; everything downstream (CaptureError, Recover, the middleware) then finds
// it with errors.As without any extra call, and errors.Is/errors.As on the original error still
// work through it. A custom error type can also expose the statement itself by implementing
// SQLStatement() string, with no need to call WithSQL at all.
//
//	rows, err := db.QueryContext(ctx, query, args...)
//	if err != nil {
//	    return forgeops.WithSQL(err, query)
//	}
func WithSQL(err error, statement string) error {
	if err == nil {
		return nil
	}
	return &sqlError{err: err, statement: statement}
}

type sqlError struct {
	err       error
	statement string
}

func (e *sqlError) Error() string        { return e.err.Error() }
func (e *sqlError) Unwrap() error        { return e.err }
func (e *sqlError) SQLStatement() string { return e.statement }

type sqlStatementer interface{ SQLStatement() string }

// findSQL returns the raw statement carried by err or anything it wraps, or "" when none does.
func findSQL(err error) string {
	queue := []error{err}
	for visited := 0; len(queue) > 0 && visited < maxCauseDepth*3; visited++ {
		current := queue[0]
		queue = queue[1:]
		if current == nil {
			continue
		}
		var carrier sqlStatementer
		if errors.As(current, &carrier) {
			if statement := carrier.SQLStatement(); strings.TrimSpace(statement) != "" {
				return statement
			}
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() error }:
			queue = append(queue, wrapped.Unwrap())
		case interface{ Unwrap() []error }:
			queue = append(queue, wrapped.Unwrap()...)
		}
	}
	return ""
}

func isWordByte(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isDigitByte(c byte) bool { return c >= '0' && c <= '9' }

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

// maskSQL replaces every string literal and number in statement with "?". system is the
// statement's db.system when known ("" otherwise): for "mysql" or "mariadb", in any case, "double
// quoted" text is a string there and is masked too. Returns "" for a blank statement.
func maskSQL(statement, system string) string {
	if strings.TrimSpace(statement) == "" {
		return ""
	}
	doubleQuotes := strings.EqualFold(system, "mysql") || strings.EqualFold(system, "mariadb")

	var out strings.Builder
	s := statement
	for i := 0; i < len(s); {
		if end := sqlLiteralEnd(s, i, doubleQuotes); end >= 0 {
			out.WriteString(sqlMask)
			i = end
			continue
		}
		out.WriteByte(s[i])
		i++
	}

	masked := out.String()
	if runes := []rune(masked); len(runes) > maxSQLLength {
		masked = string(runes[:maxSQLLength]) + "..."
	}
	return masked
}

// sqlLiteralEnd returns where the literal starting at i ends, or -1 when none starts there. Where
// the shared pattern would fail its lookbehind (a prefix or number that's part of a word), this
// returns -1 and maskSQL simply moves on one byte, which is what the pattern does too.
func sqlLiteralEnd(s string, i int, doubleQuotes bool) int {
	c := s[i]
	partOfWord := i > 0 && (isWordByte(s[i-1]) || s[i-1] == '$')
	switch {
	case c == '\'':
		return quotedEnd(s, i, '\'')

	case c == '"' && doubleQuotes:
		return quotedEnd(s, i, '"')

	case c == '$':
		// A dollar-quoted body ($tag$ ... $tag$): PostgreSQL function bodies and DO blocks.
		j := i + 1
		for j < len(s) && (s[j] == '_' || (s[j] >= 'a' && s[j] <= 'z') || (s[j] >= 'A' && s[j] <= 'Z')) {
			j++
		}
		if j < len(s) && s[j] == '$' {
			tag := s[i : j+1]
			if idx := strings.Index(s[j+1:], tag); idx >= 0 {
				return j + 1 + idx + len(tag)
			}
			return len(s)
		}

	case isDigitByte(c) || c == '.':
		// A number, unless it's part of an identifier (orders2, sp_v2), a $1 placeholder, or the
		// fraction of another number; those digits are left alone.
		if partOfWord || (i > 0 && s[i-1] == '.') {
			return -1
		}
		return numberEnd(s, i)

	default:
		// A string's type prefix (E'', X'', N'', B'', U&'') is masked along with it, unless the
		// letter is really the end of a word: LIKE'%x%' keeps its E, and the quote still starts
		// a string on the next byte.
		prefix := 0
		if strings.IndexByte("EeXxNnBb", c) >= 0 {
			prefix = 1
		} else if (c == 'U' || c == 'u') && i+1 < len(s) && s[i+1] == '&' {
			prefix = 2
		}
		if prefix > 0 && !partOfWord && i+prefix < len(s) && s[i+prefix] == '\'' {
			return quotedEnd(s, i+prefix, '\'')
		}
	}
	return -1
}

// quotedEnd returns where the string opened by the quote q at i ends. A quote written twice, or a
// backslash before any character ('o\'brien'), doesn't end it, and a string cut off by truncation
// (no closing quote) is masked to the end of the statement, never left half-visible, including
// one cut off right after a backslash.
func quotedEnd(s string, i int, q byte) int {
	for j := i + 1; j < len(s); {
		switch s[j] {
		case '\\':
			j += 2
			continue
		case q:
			if j+1 < len(s) && s[j+1] == q {
				j += 2
				continue
			}
			return j + 1
		default:
			j++
		}
	}
	return len(s)
}

// numberEnd returns where the number starting at i ends, or -1 when it isn't a standalone number
// (immediately followed by a letter, digit or underscore). Hex (0x1F) and binary (0b101) come
// first; otherwise an integer or decimal (or a leading-dot .5), with an optional exponent (3e10,
// 1.5E-3). A form that fails the standalone check falls back to a shorter one (1.5e3x to 1.5, then
// to 1) the same way the shared pattern's backtracking does.
func numberEnd(s string, i int) int {
	standalone := func(end int) bool { return end >= len(s) || !isWordByte(s[end]) }

	if end := radixNumberEnd(s, i, 'x', isHexByte); end >= 0 && standalone(end) {
		return end
	}
	if end := radixNumberEnd(s, i, 'b', isBinaryByte); end >= 0 && standalone(end) {
		return end
	}

	var bases []int
	if isDigitByte(s[i]) {
		k := digitsEnd(s, i)
		if k+1 < len(s) && s[k] == '.' && isDigitByte(s[k+1]) {
			bases = append(bases, digitsEnd(s, k+1))
		}
		bases = append(bases, k)
	} else if i+1 < len(s) && isDigitByte(s[i+1]) {
		bases = append(bases, digitsEnd(s, i+1))
	}
	for _, base := range bases {
		if end := exponentEnd(s, base); end >= 0 && standalone(end) {
			return end
		}
		if standalone(base) {
			return base
		}
	}
	return -1
}

// radixNumberEnd returns where a 0x (or 0b) number starting at i ends, or -1 if none starts there;
// letter is the lowercase marker, and either case matches.
func radixNumberEnd(s string, i int, letter byte, digit func(byte) bool) int {
	if s[i] != '0' || i+2 >= len(s) || (s[i+1]|0x20) != letter || !digit(s[i+2]) {
		return -1
	}
	k := i + 2
	for k < len(s) && digit(s[k]) {
		k++
	}
	return k
}

// exponentEnd returns where an exponent (e10, E-3, e+2) starting at k ends, or -1 if none does.
func exponentEnd(s string, k int) int {
	if k >= len(s) || (s[k] != 'e' && s[k] != 'E') {
		return -1
	}
	m := k + 1
	if m < len(s) && (s[m] == '+' || s[m] == '-') {
		m++
	}
	if m >= len(s) || !isDigitByte(s[m]) {
		return -1
	}
	return digitsEnd(s, m)
}

func digitsEnd(s string, k int) int {
	for k < len(s) && isDigitByte(s[k]) {
		k++
	}
	return k
}

func isHexByte(c byte) bool {
	return isDigitByte(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func isBinaryByte(c byte) bool { return c == '0' || c == '1' }

type sqlToken struct {
	text   string
	isName bool
}

// namePartEnd returns where the identifier part starting at i ends, or -1 if none starts there:
// bare (letters, digits, _ $ # @), "double quoted", [bracketed] (SQL Server) or `backticked`.
func namePartEnd(s string, i int) int {
	if i >= len(s) {
		return -1
	}
	c := s[i]
	switch {
	case isWordByte(c) || c == '$' || c == '#' || c == '@':
		j := i
		for j < len(s) && (isWordByte(s[j]) || s[j] == '$' || s[j] == '#' || s[j] == '@') {
			j++
		}
		return j
	case c == '"' || c == '`' || c == '[':
		closer := c
		if c == '[' {
			closer = ']'
		}
		j := i + 1
		for j < len(s) && s[j] != closer {
			j++
		}
		if j < len(s) && j > i+1 {
			return j + 1
		}
	}
	return -1
}

// nameEnd returns where the (optionally schema-qualified) name starting at i ends, or -1.
func nameEnd(s string, i int) int {
	end := namePartEnd(s, i)
	if end < 0 {
		return -1
	}
	for end < len(s) && s[end] == '.' {
		next := namePartEnd(s, end+1)
		if next < 0 {
			break
		}
		end = next
	}
	return end
}

func tokenizeSQL(s string) []sqlToken {
	var tokens []sqlToken
	for i := 0; i < len(s); {
		if isSpaceByte(s[i]) {
			i++
			continue
		}
		if end := nameEnd(s, i); end >= 0 {
			tokens = append(tokens, sqlToken{text: s[i:end], isName: true})
			i = end
			continue
		}
		tokens = append(tokens, sqlToken{text: s[i : i+1]})
		i++
	}
	return tokens
}

func isFullName(s string) bool {
	return s != "" && nameEnd(s, 0) == len(s)
}

var (
	sqlOperations = map[string]bool{
		"SELECT": true, "INSERT": true, "UPDATE": true, "DELETE": true, "MERGE": true, "WITH": true, "CALL": true,
		"EXEC": true, "EXECUTE": true, "CREATE": true, "ALTER": true, "DROP": true, "TRUNCATE": true,
	}
	sqlBuiltins = map[string]bool{
		"count": true, "sum": true, "min": true, "max": true, "avg": true, "now": true, "coalesce": true,
		"nullif": true, "lower": true, "upper": true, "length": true, "concat": true, "cast": true,
		"date_trunc": true, "current_timestamp": true, "current_date": true, "row_number": true, "rank": true,
		"json_build_object": true, "json_agg": true, "array_agg": true,
	}
	sqlKeywordsNotNames = map[string]bool{
		"select": true, "set": true, "values": true, "where": true, "lateral": true, "only": true,
		"unnest": true, "generate_series": true,
	}
	sqlProcedureKeywords   = map[string]bool{"call": true, "exec": true, "execute": true, "perform": true}
	sqlNotProcedureNames   = map[string]bool{"immediate": true, "function": true, "procedure": true}
	sqlRelationKeywords    = map[string]bool{"from": true, "join": true, "into": true, "update": true, "table": true}
	sqlFunctionLikeSkipped = map[string]bool{"extract": true, "substring": true, "trim": true, "overlay": true}
)

// extractSQLObjects takes an already-masked statement (so a keyword inside a string value can't
// be mistaken for SQL) and returns the names it touched, or nil when nothing recognizable was
// found.
func extractSQLObjects(masked string) *SQLObjects {
	if strings.TrimSpace(masked) == "" {
		return nil
	}

	// EXTRACT(year FROM col), SUBSTRING(x FROM 2), TRIM(BOTH FROM x): a FROM that isn't a table.
	var tokens []sqlToken
	all := tokenizeSQL(masked)
	for i := 0; i < len(all); i++ {
		t := all[i]
		if t.isName && sqlFunctionLikeSkipped[strings.ToLower(t.text)] && i+1 < len(all) && all[i+1].text == "(" {
			closeAt := -1
			for j := i + 2; j < len(all); j++ {
				if all[j].text == "(" {
					break
				}
				if all[j].text == ")" {
					closeAt = j
					break
				}
			}
			if closeAt >= 0 {
				i = closeAt
				continue
			}
		}
		tokens = append(tokens, t)
	}

	var procedures, relations []string

	for i := 0; i+1 < len(tokens); i++ {
		if tokens[i].isName && sqlProcedureKeywords[strings.ToLower(tokens[i].text)] && tokens[i+1].isName {
			name := tokens[i+1].text
			first := strings.ToLower(strings.SplitN(name, ".", 2)[0])
			if sqlNotProcedureNames[first] {
				continue
			}
			procedures = append(procedures, name)
			i++
		}
	}

	for i := 0; i+1 < len(tokens); i++ {
		if !tokens[i].isName || !sqlRelationKeywords[strings.ToLower(tokens[i].text)] || !tokens[i+1].isName {
			continue
		}
		keyword := strings.ToLower(tokens[i].text)
		name := tokens[i+1].text
		paren := i+2 < len(tokens) && tokens[i+2].text == "("
		i++
		if sqlKeywordsNotNames[strings.ToLower(name)] {
			continue
		}
		// FROM/JOIN some_function(...) is a set-returning function (often a stored one), not a
		// table. INSERT INTO t (a, b) is just a column list, so INTO/UPDATE/TABLE never count.
		if paren && (keyword == "from" || keyword == "join") {
			procedures = append(procedures, name)
		} else {
			relations = append(relations, name)
		}
	}

	if len(tokens) >= 3 && tokens[0].isName && strings.EqualFold(tokens[0].text, "select") &&
		tokens[1].isName && tokens[2].text == "(" && !sqlBuiltins[strings.ToLower(tokens[1].text)] {
		hasFrom := false
		for _, t := range tokens {
			if t.isName && strings.EqualFold(t.text, "from") {
				hasFrom = true
				break
			}
		}
		if !hasFrom {
			procedures = append(procedures, tokens[1].text)
		}
	}

	operation := ""
	if len(tokens) > 0 {
		word := tokens[0].text
		end := 0
		for end < len(word) && isWordByte(word[end]) {
			end++
		}
		if candidate := strings.ToUpper(word[:end]); sqlOperations[candidate] {
			operation = candidate
		}
	}

	objects := &SQLObjects{Operation: operation, Procedures: cleanSQLNames(procedures), Relations: cleanSQLNames(relations)}
	if len(objects.Procedures) == 0 && len(objects.Relations) == 0 && objects.Operation == "" {
		return nil
	}
	return objects
}

func cleanSQLNames(names []string) []string {
	cleaned := []string{}
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if runes := []rune(name); len(runes) > maxNameLength {
			name = string(runes[:maxNameLength])
		}
		if !isFullName(name) {
			continue
		}
		duplicate := false
		for _, existing := range cleaned {
			if existing == name {
				duplicate = true
				break
			}
		}
		if !duplicate {
			cleaned = append(cleaned, name)
		}
	}
	if len(cleaned) > maxSQLNames {
		cleaned = cleaned[:maxSQLNames]
	}
	return cleaned
}

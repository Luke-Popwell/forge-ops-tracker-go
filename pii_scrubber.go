// Redacts likely-sensitive content out of a payload before it ever leaves this process: the
// same patterns ForgeOps itself applies again on arrival (defense in depth: this layer keeps the
// data off the wire and out of any request logging in between; the server-side layer is what
// actually protects the database, and doesn't depend on every reporting app running an up-to-date
// version of this client). Ported from
// gems/forge_ops_tracker/lib/forge_ops_tracker/pii_scrubber.rb: kept dependency-free here for
// the same reason as the Ruby original: this has to work in any host app regardless of what's
// reporting into it.
//
// Can be turned off via Configuration.ScrubPII = false for a host app that already scrubs its own
// data before it ever reaches error context, or that has its own reasons to want the raw payload.
// Off by default is not an option: the safe default has to be "on."
package forgeops

import (
	"regexp"
	"strings"
)

const redacted = "[FILTERED]"

var sensitiveKeys = map[string]bool{
	"password": true, "passwd": true, "pwd": true,
	"secret": true, "apisecret": true, "clientsecret": true, "secretkey": true,
	"token": true, "accesstoken": true, "refreshtoken": true, "apikey": true, "apitoken": true,
	"authorization": true, "authtoken": true, "bearer": true,
	"sessiontoken": true, "csrftoken": true,
	"creditcard": true, "cardnumber": true, "cardnum": true, "cvv": true, "cvv2": true, "cvc": true,
	"iban": true, "accountnumber": true, "routingnumber": true, "bankaccount": true,
	"ssn": true, "socialsecuritynumber": true, "socialsecurity": true,
	"privatekey": true,
}

type scrubPattern struct {
	label   string
	pattern *regexp.Regexp
}

var scrubPatterns = []scrubPattern{
	{"EMAIL", regexp.MustCompile(`[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}`)},
	{"SSN", regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)},
	{"CREDIT CARD", regexp.MustCompile(`\b\d{4}[ -]\d{4}[ -]\d{4}[ -]\d{1,4}\b`)},
	{"BEARER TOKEN", regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9\-._~+/]+=*`)},
	{"JWT", regexp.MustCompile(`\bey[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`)},
	{"AWS KEY", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"STRIPE KEY", regexp.MustCompile(`\b[sr]k_(?:live|test)_[A-Za-z0-9]{10,}\b`)},
	{"GITHUB TOKEN", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`)},
}

// IBAN candidates: two letters, two check digits and 11 to 30 more letters or digits, compact or in
// groups of four. Go's regexp picks the same leftmost match a backtracking engine would here, and
// its \b and \d are ASCII, as sdks/PII_SCRUBBING.md asks. A candidate is scrubbed only when the
// IBAN check passes (isIBAN).
var ibanPattern = regexp.MustCompile(`\b[A-Z]{2}\d{2}(?:[A-Z0-9]{11,30}|(?: [A-Z0-9]{4}){2,7}(?: [A-Z0-9]{1,4})?)\b`)

// ScrubString runs every pattern above over a single string, independent of any key: used both
// directly (a message, a stack frame's file/method) and as the leaf case of ScrubValue below.
// IBANs go first (a spaced IBAN's middle groups look like a formatted card number), then the
// patterns above, then card numbers in any other shape. sdks/PII_SCRUBBING.md is the rule and
// testdata/pii_scrubbing_vectors.json pins it.
func ScrubString(text string) string {
	text = ibanPattern.ReplaceAllStringFunc(text, func(candidate string) string {
		if isIBAN(candidate) {
			return "[IBAN FILTERED]"
		}
		return candidate
	})
	for _, p := range scrubPatterns {
		text = p.pattern.ReplaceAllString(text, "["+p.label+" FILTERED]")
	}
	return scrubCardNumbers(text)
}

// isWordByte and isDigitByte (ASCII only) are in sql_statement.go.
//
// scrubCardNumbers finds what (?<![A-Za-z0-9_])\d(?:[ -]?\d)*(?![A-Za-z0-9_]) would, by hand (Go's
// regexp has no lookaround): a run of digits joined by single spaces or dashes, starting at a digit
// not preceded by a word character. When the full run is followed by a word character, a regex
// engine backs off to just before the run's last separator (a separator is not a word character);
// with no separator to back off to, the run is no match at all and scanning carries on after it.
func scrubCardNumbers(text string) string {
	var out strings.Builder
	copied := 0
	n := len(text)
	for i := 0; i < n; {
		if !isDigitByte(text[i]) || (i > 0 && isWordByte(text[i-1])) {
			i++
			continue
		}
		end, lastSeparator := i+1, -1
		for end < n {
			if isDigitByte(text[end]) {
				end++
			} else if (text[end] == ' ' || text[end] == '-') && end+1 < n && isDigitByte(text[end+1]) {
				lastSeparator = end
				end += 2
			} else {
				break
			}
		}
		if end < n && isWordByte(text[end]) {
			if lastSeparator < 0 {
				i = end
				continue
			}
			end = lastSeparator
		}
		out.WriteString(text[copied:i])
		out.WriteString(scrubCardNumbersIn(text[i:end]))
		copied, i = end, end
	}
	if copied == 0 {
		return text
	}
	out.WriteString(text[copied:])
	return out.String()
}

// scrubCardNumbersIn splits a run into its groups at each space or dash. From each group on, the
// longest stretch of whole groups that is a card number is replaced, and scanning carries on after
// it; with none, from the next group. So a card followed by its CVV ("4242424242424242 123"), two
// cards in a row, and Amex's 4-6-5 grouping are all found.
func scrubCardNumbersIn(run string) string {
	var groups, separators []string
	start := 0
	for k := 0; k < len(run); k++ {
		if run[k] == ' ' || run[k] == '-' {
			groups = append(groups, run[start:k])
			separators = append(separators, run[k:k+1])
			start = k + 1
		}
	}
	groups = append(groups, run[start:])
	separators = append(separators, "")

	var out strings.Builder
	for i := 0; i < len(groups); {
		// A card number has at most 19 digits, so only stretches that short can be one; the last
		// one that is wins, the same as trying the longest stretch first.
		found, joined := -1, ""
		for last := i; last < len(groups) && len(joined)+len(groups[last]) <= 19; last++ {
			joined += groups[last]
			if isCardNumber(joined) {
				found = last
			}
		}
		if found >= 0 {
			out.WriteString("[CREDIT CARD FILTERED]")
			out.WriteString(separators[found])
			i = found + 1
		} else {
			out.WriteString(groups[i])
			out.WriteString(separators[i])
			i++
		}
	}
	return out.String()
}

// isCardNumber: 13 to 19 digits, a card network's prefix, and the Luhn check.
func isCardNumber(digits string) bool {
	if len(digits) < 13 || len(digits) > 19 || !hasCardPrefix(digits) {
		return false
	}
	sum := 0
	for k := 0; k < len(digits); k++ {
		value := int(digits[len(digits)-1-k] - '0')
		if k%2 == 1 {
			value *= 2
			if value > 9 {
				value -= 9
			}
		}
		sum += value
	}
	return sum%10 == 0
}

// hasCardPrefix: 4, 51 to 55, 2221 to 2720, 34, 37, 6011, 644 to 649, 65, 3528 to 3589, 36,
// 300 to 305, 62. Only called with at least 13 digits.
func hasCardPrefix(digits string) bool {
	two := int(digits[0]-'0')*10 + int(digits[1]-'0')
	three := two*10 + int(digits[2]-'0')
	four := three*10 + int(digits[3]-'0')
	switch {
	case digits[0] == '4':
		return true
	case two >= 51 && two <= 55, two == 34, two == 37, two == 65, two == 36, two == 62:
		return true
	case four >= 2221 && four <= 2720, four == 6011, four >= 3528 && four <= 3589:
		return true
	case three >= 644 && three <= 649, three >= 300 && three <= 305:
		return true
	}
	return false
}

// isIBAN is the IBAN check, a digit at a time so no big integers are needed: with spaces removed,
// 15 to 34 characters; the first four moved to the end, each letter read as two digits (A is 10,
// Z is 35), and the remainder by 97 is 1.
func isIBAN(candidate string) bool {
	compact := strings.ReplaceAll(candidate, " ", "")
	if len(compact) < 15 || len(compact) > 34 {
		return false
	}
	rearranged := compact[4:] + compact[:4]
	remainder := 0
	for k := 0; k < len(rearranged); k++ {
		c := rearranged[k]
		if c >= 'A' && c <= 'Z' {
			value := int(c-'A') + 10
			remainder = (remainder*10 + value/10) % 97
			remainder = (remainder*10 + value%10) % 97
		} else {
			remainder = (remainder*10 + int(c-'0')) % 97
		}
	}
	return remainder == 1
}

// ScrubValue redacts value based on key (an entire value redacted wholesale if key looks
// sensitive, regardless of type) and recurses into maps/slices, matching every other client's
// behavior in this repo.
func ScrubValue(value any, key string) any {
	if value != nil && isSensitiveKey(key) {
		return redacted
	}

	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, inner := range v {
			out[k] = ScrubValue(inner, k)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, inner := range v {
			out[i] = ScrubValue(inner, key)
		}
		return out
	case string:
		return ScrubString(v)
	default:
		return value
	}
}

func isSensitiveKey(key string) bool {
	if key == "" {
		return false
	}
	var b strings.Builder
	for _, r := range strings.ToLower(key) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	normalized := b.String()
	for sensitive := range sensitiveKeys {
		if strings.Contains(normalized, sensitive) {
			return true
		}
	}
	return false
}

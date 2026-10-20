package forgeops

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Strict privacy (Configuration.StrictPrivacy) is for apps that can't let personal or health data
// leave their own servers: an event carries only structured facts (the error's type, its stack
// frames, release, environment, server, which route it happened in), never free text a person
// could have typed. The rules, and the vectors every client's tests check against, are in this
// repo's sdks/STRICT_PRIVACY.md; the server applies the same rules again on arrival
// (app/services/strict_privacy.rb) when the project's own Strict privacy setting is on.

// fingerprintLength is how many hex characters of the SHA-256 digest message_fingerprint keeps.
const fingerprintLength = 32

// strictSpanDataKeys is the span data that's structure, not content: the masked statement and
// which database it ran on.
var strictSpanDataKeys = []string{"db.statement", "db.system"}

// strictHTTPMethod matches a span name that names an outbound call: "GET https://host/path".
var strictHTTPMethod = regexp.MustCompile(`(?i)^(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS)\s+(\S+)`)

// strictPrivacyFromEnvironment reads FORGE_OPS_STRICT_PRIVACY and FORGE_OPS_PRIVACY_ALLOWED_FIELDS
// through getenv, for NewConfiguration's defaults. Anything set on the Configuration afterwards wins.
func strictPrivacyFromEnvironment(getenv func(string) string) (bool, []string) {
	on := strings.EqualFold(strings.TrimSpace(getenv("FORGE_OPS_STRICT_PRIVACY")), "true")
	var fields []string
	for _, field := range strings.Split(getenv("FORGE_OPS_PRIVACY_ALLOWED_FIELDS"), ",") {
		if field = strings.TrimSpace(field); field != "" {
			fields = append(fields, field)
		}
	}
	return on, fields
}

func defaultStrictPrivacy() (bool, []string) {
	return strictPrivacyFromEnvironment(os.Getenv)
}

// normalizeMessage returns message with hex object ids, UUIDs and standalone numbers replaced, in
// that order: "0x" plus 4 or more hex digits becomes "0x", a UUID becomes "<uuid>" and a run of
// digits becomes "<n>", the last two only where no ASCII word character ([A-Za-z0-9_]) touches
// them. RE2 has no lookbehind, so each step is a scan that checks the neighbouring bytes by hand.
// Every pattern byte is ASCII and no byte of a multi-byte UTF-8 character is, so scanning bytes
// never splits or misreads a character.
func normalizeMessage(message string) string {
	return replaceNumbers(replaceUUIDs(replaceHexObjectIDs(message)))
}

// messageFingerprint is the first 32 hex characters of the SHA-256 of normalizeMessage(message):
// lets ForgeOps group errors whose messages differ only in ids, without the message being sent.
func messageFingerprint(message string) string {
	sum := sha256.Sum256([]byte(normalizeMessage(message)))
	return hex.EncodeToString(sum[:])[:fingerprintLength]
}

func isASCIIWord(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func isDigit(c byte) bool {
	return c >= '0' && c <= '9'
}

func replaceHexObjectIDs(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '0' && i+1 < len(s) && s[i+1] == 'x' {
			end := i + 2
			for end < len(s) && isHexDigit(s[end]) {
				end++
			}
			if end-(i+2) >= 4 {
				out.WriteString("0x")
				i = end
				continue
			}
		}
		out.WriteByte(s[i])
		i++
	}
	return out.String()
}

// uuidGroups is the 8-4-4-4-12 layout of a UUID's hex digits.
var uuidGroups = []int{8, 4, 4, 4, 12}

func uuidAt(s string, i int) bool {
	for g, length := range uuidGroups {
		if g > 0 {
			if i >= len(s) || s[i] != '-' {
				return false
			}
			i++
		}
		for n := 0; n < length; n++ {
			if i >= len(s) || !isHexDigit(s[i]) {
				return false
			}
			i++
		}
	}
	return true
}

func replaceUUIDs(s string) string {
	const uuidLength = 36
	var out strings.Builder
	for i := 0; i < len(s); {
		if (i == 0 || !isASCIIWord(s[i-1])) && uuidAt(s, i) && (i+uuidLength == len(s) || !isASCIIWord(s[i+uuidLength])) {
			out.WriteString("<uuid>")
			i += uuidLength
			continue
		}
		out.WriteByte(s[i])
		i++
	}
	return out.String()
}

func replaceNumbers(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); {
		if isDigit(s[i]) && (i == 0 || !isASCIIWord(s[i-1])) {
			end := i
			for end < len(s) && isDigit(s[end]) {
				end++
			}
			if end == len(s) || !isASCIIWord(s[end]) {
				out.WriteString("<n>")
				i = end
				continue
			}
		}
		out.WriteByte(s[i])
		i++
	}
	return out.String()
}

// pathWithoutIDs drops a path's query and fragment and replaces every segment that holds an ASCII
// digit, or is longer than 40 characters, with ":id": "/patients/42/notes?q=x" is
// "/patients/:id/notes". An empty result is "/".
func pathWithoutIDs(path string) string {
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	segments := strings.Split(path, "/")
	// Trailing empty segments are dropped, so "/orders/" and "/orders" read the same, matching the
	// server's own split.
	for len(segments) > 0 && segments[len(segments)-1] == "" {
		segments = segments[:len(segments)-1]
	}
	for i, segment := range segments {
		if strings.IndexAny(segment, "0123456789") >= 0 || utf8.RuneCountInString(segment) > 40 {
			segments[i] = ":id"
		}
	}
	if joined := strings.Join(segments, "/"); joined != "" {
		return joined
	}
	return "/"
}

// strictSpanName is a span's name under strict privacy: an outbound call keeps its method and host
// ("GET https://api.example.com/patients/42?x=1" is "GET api.example.com"), a request named by its
// path loses the ids in it ("GET /patients/42/notes" is "GET /patients/:id/notes"), so does a bare
// path ("/patients/42" is "/patients/:id"), and any other name ("OrdersController#create") is kept.
// The host is read by hand, never with a URL parser, so an address a parser would reject still
// loses its path.
func strictSpanName(name string) string {
	if strings.HasPrefix(name, "/") {
		return pathWithoutIDs(name)
	}
	match := strictHTTPMethod.FindStringSubmatch(name)
	if match == nil {
		return name
	}
	method, target := strings.ToUpper(match[1]), match[2]
	if !strings.HasPrefix(target, "/") {
		if host, ok := strictHost(target); ok {
			return method + " " + host
		}
	}
	return method + " " + pathWithoutIDs(target)
}

// strictTransactionName is a transaction name under strict privacy: a route or a controller action
// is kept, but a raw path loses its ids, by the same rule as a span's name.
func strictTransactionName(name string) string {
	if strings.TrimSpace(name) == "" {
		return name
	}
	return strictSpanName(name)
}

var (
	strictScheme = regexp.MustCompile(`(?i)^[a-z][a-z0-9+.-]*://`)
	strictPort   = regexp.MustCompile(`:[0-9]+$`)
)

// strictHost is an outbound call's host: whatever comes before the first "/", "?" or "#", without
// a scheme, credentials or port. It counts as a host when the address had a scheme or a port, or
// the host has a dot or is "localhost"; otherwise (a relative path) ok is false.
func strictHost(target string) (host string, ok bool) {
	scheme := strictScheme.MatchString(target)
	authority := strictScheme.ReplaceAllString(target, "")
	if i := strings.IndexAny(authority, "/?#"); i >= 0 {
		authority = authority[:i]
	}
	if i := strings.LastIndexByte(authority, '@'); i >= 0 {
		authority = authority[i+1:]
	}
	port := strictPort.MatchString(authority)
	host = strictPort.ReplaceAllString(authority, "")
	if host == "" {
		return "", false
	}
	return host, scheme || port || strings.Contains(host, ".") || strings.EqualFold(host, "localhost")
}

// applyStrictPrivacy reduces a built error event to what strict privacy sends: the message is
// replaced by its fingerprint; breadcrumbs, endpoint, sql_statement and source context are dropped;
// context and tags keep only allowed keys with plain values; user keeps only its id.
func applyStrictPrivacy(payload map[string]any, allowedFields []string) map[string]any {
	message, _ := payload["message"].(string)
	payload["message"] = ""
	payload["message_fingerprint"] = messageFingerprint(message)
	delete(payload, "breadcrumbs")
	delete(payload, "endpoint")
	delete(payload, "sql_statement")
	if name, ok := payload["transaction_name"].(string); ok {
		payload["transaction_name"] = strictTransactionName(name)
	}

	for _, key := range []string{"context", "tags"} {
		if kept := allowedStrictFields(payload[key], allowedFields); kept != nil {
			payload[key] = kept
		} else {
			delete(payload, key)
		}
	}

	if id := strictUserID(payload["user"]); id != "" {
		payload["user"] = map[string]any{"id": id}
	} else {
		delete(payload, "user")
	}

	if frames, ok := payload["backtrace"].([]Frame); ok {
		stripped := make([]Frame, len(frames))
		for i, frame := range frames {
			stripped[i] = Frame{File: frame.File, Line: frame.Line, Method: frame.Method, InApp: frame.InApp}
		}
		payload["backtrace"] = stripped
	}
	return payload
}

// allowedStrictFields is fields' top-level allowed keys whose value is a string, number or boolean,
// or nil when none are left.
func allowedStrictFields(fields any, allowedFields []string) map[string]any {
	values, ok := fields.(map[string]any)
	if !ok || len(allowedFields) == 0 {
		return nil
	}
	kept := map[string]any{}
	for _, key := range allowedFields {
		if value, present := values[key]; present && isPlainValue(value) {
			kept[key] = value
		}
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

func isPlainValue(value any) bool {
	if value == nil {
		return false
	}
	switch reflect.TypeOf(value).Kind() {
	case reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

// strictUserID is the user's "id" as a string, or "" when there is none (or it's blank).
func strictUserID(user any) string {
	values, ok := user.(map[string]any)
	if !ok || values["id"] == nil {
		return ""
	}
	id := fmt.Sprint(values["id"])
	if strings.TrimSpace(id) == "" {
		return ""
	}
	return id
}

// strictSpan is one recorded span under strict privacy: its name per strictSpanName, and its data
// reduced to db.statement and db.system (left out when neither is there).
func strictSpan(span map[string]any) map[string]any {
	out := make(map[string]any, len(span))
	for key, value := range span {
		out[key] = value
	}
	if name, ok := span["name"].(string); ok {
		out["name"] = strictSpanName(name)
	}
	delete(out, "data")
	if data, ok := span["data"].(map[string]any); ok {
		kept := map[string]any{}
		for _, key := range strictSpanDataKeys {
			if value, present := data[key]; present {
				kept[key] = value
			}
		}
		if len(kept) > 0 {
			out["data"] = kept
		}
	}
	return out
}

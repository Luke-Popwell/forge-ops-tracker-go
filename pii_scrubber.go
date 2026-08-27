// Redacts likely-sensitive content out of a payload before it ever leaves this process -- the
// same patterns ForgeOps itself applies again on arrival (defense in depth: this layer keeps the
// data off the wire and out of any request logging in between; the server-side layer is what
// actually protects the database, and doesn't depend on every reporting app running an up-to-date
// version of this client). Ported from
// gems/forge_ops_tracker/lib/forge_ops_tracker/pii_scrubber.rb -- kept dependency-free here for
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

// ScrubString runs every pattern above over a single string, independent of any key -- used both
// directly (a message, a stack frame's file/method) and as the leaf case of ScrubValue below.
func ScrubString(text string) string {
	for _, p := range scrubPatterns {
		text = p.pattern.ReplaceAllString(text, "["+p.label+" FILTERED]")
	}
	return text
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

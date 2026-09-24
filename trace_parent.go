package forgeops

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strings"
	"time"
)

// TraceParentHeader is the W3C Trace Context header (https://www.w3.org/TR/trace-context/) that
// carries one trace across service boundaries: "00-<32 hex trace id>-<16 hex parent span id>-<2 hex
// flags>". WithRequest reads it off an incoming request to continue the caller's trace, and
// Transport sets it on outbound requests so the next service continues this one.
const TraceParentHeader = "traceparent"

var traceParentPattern = regexp.MustCompile(`^([0-9a-f]{2})-([0-9a-f]{32})-([0-9a-f]{16})-([0-9a-f]{2})(-.*)?$`)

const (
	invalidTraceID = "00000000000000000000000000000000"
	invalidSpanID  = "0000000000000000"

	// Always "01" (sampled) on the way out: whether a trace is sent is only decided once it's over
	// (see Configuration.TraceCaptureThreshold), long after this header has gone out, so "this may be
	// recorded" is the only honest answer. The next service makes its own decision either way.
	sampledFlags = "01"
)

// parseTraceParent reads a traceparent header value, mirroring gems/forge_ops_tracker's own
// TraceParent.parse. Strict on the way in, the posture the spec asks receivers to take: a malformed
// value, uppercase hex, the reserved version ff, or an all-zero trace or parent id all mean "no
// usable header" (ok is false and a fresh trace starts), never half-trusted. A future version is
// still accepted when its first four fields have version 00's shape; version 00 itself must have
// exactly four.
func parseTraceParent(value string) (traceID, parentSpanID string, ok bool) {
	match := traceParentPattern.FindStringSubmatch(strings.TrimSpace(value))
	if match == nil {
		return "", "", false
	}
	version, traceID, parentSpanID, rest := match[1], match[2], match[3], match[5]
	if version == "ff" || (version == "00" && rest != "") {
		return "", "", false
	}
	if traceID == invalidTraceID || parentSpanID == invalidSpanID {
		return "", "", false
	}
	return traceID, parentSpanID, true
}

func buildTraceParent(traceID, spanID string) string {
	return "00-" + traceID + "-" + spanID + "-" + sampledFlags
}

// generateTraceID returns 32 lowercase hex characters, never all zeros (the spec's one invalid value).
func generateTraceID() string {
	return randomNonZeroHex(16)
}

// generateSpanID returns 16 lowercase hex characters, never all zeros.
func generateSpanID() string {
	return randomNonZeroHex(8)
}

func randomNonZeroHex(n int) string {
	b := make([]byte, n)
	for {
		if _, err := rand.Read(b); err != nil {
			// crypto/rand failing is effectively impossible; a constant-length fallback that is still
			// unique enough within one trace beats panicking inside an error reporter's own tracing.
			return hex.EncodeToString([]byte(time.Now().Format("150405.000000000") + "0000000000"))[:n*2]
		}
		for _, v := range b {
			if v != 0 {
				return hex.EncodeToString(b)
			}
		}
	}
}

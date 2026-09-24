package forgeops

import (
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Transport wraps base (http.DefaultTransport if nil) so every outbound request made with a
// context that carries a trace (see WithTrace) is recorded as an "http" span under whatever span is
// currently open, named "<METHOD> <host>", never the full URL: a path or query string could carry
// an id or a token, the same low-cardinality, no-secrets-in-a-label reasoning every other
// transaction name in this system follows. Install it on the client you make outbound calls with:
//
//	client := &http.Client{Transport: forgeops.Transport(nil)}
//	resp, err := client.Do(req.WithContext(ctx))
//
// The request must carry the trace's own context (req.WithContext(ctx), or http.NewRequestWithContext)
// for the span to attach; a request made with a bare context.Background() records nothing. Never
// changes the caller's request, the response, or the error.
//
// Also propagates the current trace to the service being called, as a W3C traceparent header (see
// Configuration.PropagateTraces/TracePropagationTargets for turning it off or narrowing it to
// specific hosts). The header's parent id is this call's own span id, generated before the call is
// made and then recorded with that exact id, so the downstream service's root span points at a span
// that really exists in this trace. It goes out whenever the request's context belongs to a web
// request (see WithRequest), even with TrackTracing off (no span is recorded then; the trace id
// alone still links errors across services), or to a WithTrace trace. The header is set on a clone,
// since a RoundTripper must not modify the request it's given; a traceparent the request already
// carries is left alone, and this client's own requests to ForgeOps never get one.
func Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return roundTripper{base: base}
}

type roundTripper struct {
	base http.RoundTripper
}

func (t roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	traceID := TraceID(ctx)
	if traceID == "" || isOwnRequest(req) {
		return t.base.RoundTrip(req)
	}

	spanID := generateSpanID()
	req = propagate(req, traceID, spanID)

	startedAt := time.Now()
	response, err := t.base.RoundTrip(req)

	buffer := traceFromContext(ctx)
	if buffer == nil {
		return response, err
	}
	data := map[string]any{}
	if response != nil {
		data["status"] = response.StatusCode
	}
	if err != nil {
		data["error"] = true
	}
	buffer.record(spanID, parentSpanFromContext(ctx, buffer), req.Method+" "+req.URL.Host, "http", startedAt,
		float64(time.Since(startedAt))/float64(time.Millisecond), data, false)
	return response, err
}

// propagate returns req, or a clone of it carrying a traceparent header for traceID with spanID as
// the parent, per Configuration.ShouldPropagateTrace. Never panics.
func propagate(req *http.Request, traceID, spanID string) (out *http.Request) {
	out = req
	defer func() {
		if recover() != nil {
			out = req
		}
	}()
	if req.Header.Get(TraceParentHeader) != "" || req.URL == nil {
		return req
	}
	config, _ := state()
	if !config.ShouldPropagateTrace(req.URL.Hostname()) {
		return req
	}
	clone := req.Clone(req.Context())
	clone.Header.Set(TraceParentHeader, buildTraceParent(traceID, spanID))
	return clone
}

// isOwnRequest reports whether req goes to this client's own ingestion host and directory
// (/api/v1/events, /api/v1/spans, ...), so delivering a trace can never record a span of its own or
// carry a traceparent, even if someone installs Transport as http.DefaultTransport.
func isOwnRequest(req *http.Request) bool {
	if req.URL == nil {
		return false
	}
	config, _ := state()
	ingestion, err := url.Parse(config.IngestionURI())
	if err != nil || ingestion.Host == "" || !strings.EqualFold(ingestion.Host, req.URL.Host) {
		return false
	}
	directory := ingestion.Path[:strings.LastIndex(ingestion.Path, "/")+1]
	return strings.HasPrefix(req.URL.Path, directory)
}

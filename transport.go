package forgeops

import (
	"net/http"
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
// changes the request, the response, or the error.
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
	if traceFromContext(req.Context()) == nil {
		return t.base.RoundTrip(req)
	}

	startedAt := time.Now()
	response, err := t.base.RoundTrip(req)

	data := map[string]any{}
	if response != nil {
		data["status"] = response.StatusCode
	}
	if err != nil {
		data["error"] = true
	}
	RecordSpan(req.Context(), req.Method+" "+req.URL.Host, "http", startedAt, time.Since(startedAt), data)
	return response, err
}

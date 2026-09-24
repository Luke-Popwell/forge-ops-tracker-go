//go:debug httpmuxgo121=0

// The directive above opts these tests into Go 1.22+'s ServeMux (method and wildcard patterns,
// and http.Request.Pattern), which a module declaring go 1.21 otherwise gets the old behavior of:
// a host app on Go 1.22+ has it by default.

package nethttp

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	forgeops "github.com/Luke-Popwell/forge-ops-tracker-go"
)

const (
	incomingTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	incomingSpanID  = "00f067aa0ba902b7"
	incoming        = "00-" + incomingTraceID + "-" + incomingSpanID + "-01"
)

// The trace context contract end to end through both middlewares, in either order: the incoming
// traceparent is continued, errors carry the request's trace_id/transaction_name/endpoint (the
// matched ServeMux pattern, never the literal path), an errored request's trace is sent however
// fast it was, and Transport hands the trace id on to the next service.
func TestTraceContextThroughMiddlewareAndTiming(t *testing.T) {
	events := make(chan map[string]any, 8)
	traces := make(chan map[string]any, 8)
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, ok := body["message"]; ok {
			events <- body
		}
		if _, ok := body["spans"]; ok {
			traces <- body
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer tracker.Close()

	downstreamHeader := make(chan string, 1)
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downstreamHeader <- r.Header.Get("traceparent")
		w.WriteHeader(http.StatusOK)
	}))
	defer downstream.Close()

	forgeops.Init(func(c *forgeops.Configuration) {
		c.DSN = "http://key@" + tracker.Listener.Addr().String() + "/api/v1/events"
		c.Environment = "production"
		c.Timeout = time.Second
		c.TrackTracing = true
		c.TraceCaptureThreshold = 30 * time.Second // only an errored request's trace is sent
		c.PropagateTraces = true
		c.TracePropagationTargets = nil
	})

	next := func(t *testing.T, ch chan map[string]any, what string) map[string]any {
		t.Helper()
		select {
		case v := <-ch:
			return v
		case <-time.After(2 * time.Second):
			t.Fatalf("%s was never delivered", what)
			return nil
		}
	}
	rootParent := func(t *testing.T, trace map[string]any, name string) any {
		t.Helper()
		for _, s := range trace["spans"].([]any) {
			span := s.(map[string]any)
			if span["name"] == name {
				return span["parent_span_id"]
			}
		}
		t.Fatalf("no span named %q in %v", name, trace["spans"])
		return nil
	}
	serve := func(handler http.Handler, method, target string) {
		defer func() { _ = recover() }() // Middleware re-panics by design
		r := httptest.NewRequest(method, target, nil)
		r.Header.Set("traceparent", incoming)
		handler.ServeHTTP(httptest.NewRecorder(), r)
	}

	t.Run("a panic, Middleware outside Timing", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /orders/{id}", func(w http.ResponseWriter, r *http.Request) { panic("checkout failed") })
		serve(Middleware(Timing(mux)), "GET", "/orders/42")

		event := next(t, events, "the event")
		if event["trace_id"] != incomingTraceID || event["endpoint"] != "GET /orders/{id}" || event["transaction_name"] != "GET /orders/42" {
			t.Errorf("event trace fields = %v, %v, %v", event["trace_id"], event["endpoint"], event["transaction_name"])
		}
		trace := next(t, traces, "the errored request's trace")
		if trace["trace_id"] != incomingTraceID || rootParent(t, trace, "GET /orders/42") != incomingSpanID {
			t.Errorf("trace = %v, want the incoming trace id with the root under the caller's span", trace)
		}
	})

	t.Run("a handled error, Timing outside Middleware", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("POST /carts/{id}", func(w http.ResponseWriter, r *http.Request) {
			forgeops.CaptureErrorCtx(r.Context(), errors.New("bad cart"), nil, nil)
			w.WriteHeader(http.StatusUnprocessableEntity)
		})
		serve(Timing(Middleware(mux)), "POST", "/carts/9")

		event := next(t, events, "the event")
		if event["trace_id"] != incomingTraceID || event["endpoint"] != "POST /carts/{id}" {
			t.Errorf("event trace fields = %v, %v", event["trace_id"], event["endpoint"])
		}
		if trace := next(t, traces, "the errored request's trace"); trace["trace_id"] != incomingTraceID {
			t.Errorf("trace_id = %v", trace["trace_id"])
		}
	})

	t.Run("a panic seen only by Timing still sends the trace", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /boom", func(w http.ResponseWriter, r *http.Request) { panic("unreported") })
		serve(Timing(mux), "GET", "/boom")

		if trace := next(t, traces, "the errored request's trace"); trace["trace_id"] != incomingTraceID {
			t.Errorf("trace_id = %v", trace["trace_id"])
		}
	})

	t.Run("outbound calls carry the request's trace id", func(t *testing.T) {
		var seen string
		mux := http.NewServeMux()
		mux.HandleFunc("GET /checkout", func(w http.ResponseWriter, r *http.Request) {
			seen = forgeops.TraceID(r.Context())
			req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, downstream.URL+"/charge", nil)
			if resp, err := (&http.Client{Transport: forgeops.Transport(nil)}).Do(req); err == nil {
				resp.Body.Close()
			}
		})
		serve(Middleware(Timing(mux)), "GET", "/checkout")

		select {
		case header := <-downstreamHeader:
			if seen != incomingTraceID || !strings.HasPrefix(header, "00-"+incomingTraceID+"-") {
				t.Errorf("TraceID = %q, downstream traceparent = %q", seen, header)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the downstream call never arrived")
		}
		select {
		case trace := <-traces:
			t.Errorf("a fast, successful request's trace was sent: %v", trace["spans"])
		case <-time.After(150 * time.Millisecond):
		}
	})
}

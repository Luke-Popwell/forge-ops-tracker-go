package gin

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	forgeops "github.com/Luke-Popwell/forge-ops-tracker-go"
)

const (
	incomingTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	incomingSpanID  = "00f067aa0ba902b7"
	incoming        = "00-" + incomingTraceID + "-" + incomingSpanID + "-01"
)

// The trace context contract end to end through Recovery and Timing, in either order: the incoming
// traceparent is continued, errors carry the request's trace_id/transaction_name/endpoint (Gin's
// matched route, never the literal path), an errored request's trace is sent however fast it was,
// and Transport hands the trace id on to the next service.
func TestTraceContextThroughRecoveryAndTiming(t *testing.T) {
	gin.SetMode(gin.TestMode)
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
	serve := func(router *gin.Engine, method, target string) {
		r := httptest.NewRequest(method, target, nil)
		r.Header.Set("traceparent", incoming)
		router.ServeHTTP(httptest.NewRecorder(), r)
	}

	t.Run("a panic, Recovery outside Timing", func(t *testing.T) {
		router := gin.New()
		router.Use(Recovery(), Timing())
		router.GET("/orders/:id", func(c *gin.Context) { panic("checkout failed") })
		serve(router, "GET", "/orders/42")

		event := next(t, events, "the event")
		if event["trace_id"] != incomingTraceID || event["endpoint"] != "GET /orders/:id" || event["transaction_name"] != "GET /orders/:id" {
			t.Errorf("event trace fields = %v, %v, %v", event["trace_id"], event["endpoint"], event["transaction_name"])
		}
		trace := next(t, traces, "the errored request's trace")
		if trace["trace_id"] != incomingTraceID {
			t.Errorf("trace_id = %v", trace["trace_id"])
		}
		for _, s := range trace["spans"].([]any) {
			if span := s.(map[string]any); span["name"] == "GET /orders/:id" && span["parent_span_id"] != incomingSpanID {
				t.Errorf("root parent = %v, want the caller's span", span["parent_span_id"])
			}
		}
	})

	t.Run("a handled error, Timing outside Recovery", func(t *testing.T) {
		router := gin.New()
		router.Use(Timing(), Recovery())
		router.POST("/carts/:id", func(c *gin.Context) {
			forgeops.CaptureErrorCtx(c.Request.Context(), errors.New("bad cart"), nil, nil)
			c.Status(http.StatusUnprocessableEntity)
		})
		serve(router, "POST", "/carts/9")

		event := next(t, events, "the event")
		if event["trace_id"] != incomingTraceID || event["endpoint"] != "POST /carts/:id" {
			t.Errorf("event trace fields = %v, %v", event["trace_id"], event["endpoint"])
		}
		if trace := next(t, traces, "the errored request's trace"); trace["trace_id"] != incomingTraceID {
			t.Errorf("trace_id = %v", trace["trace_id"])
		}
	})

	t.Run("an unmatched route has no endpoint", func(t *testing.T) {
		router := gin.New()
		router.Use(Recovery())
		router.NoRoute(func(c *gin.Context) {
			forgeops.CaptureErrorCtx(c.Request.Context(), errors.New("missing"), nil, nil)
			c.Status(http.StatusNotFound)
		})
		serve(router, "GET", "/nowhere/7")

		event := next(t, events, "the event")
		if _, ok := event["endpoint"]; ok || event["transaction_name"] != "GET /nowhere/7" || event["trace_id"] != incomingTraceID {
			t.Errorf("event = endpoint %v, transaction_name %v, trace_id %v", event["endpoint"], event["transaction_name"], event["trace_id"])
		}
	})

	t.Run("outbound calls carry the request's trace id", func(t *testing.T) {
		router := gin.New()
		router.Use(Recovery(), Timing())
		router.GET("/checkout", func(c *gin.Context) {
			req, _ := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, downstream.URL+"/charge", nil)
			if resp, err := (&http.Client{Transport: forgeops.Transport(nil)}).Do(req); err == nil {
				resp.Body.Close()
			}
			c.Status(http.StatusOK)
		})
		serve(router, "GET", "/checkout")

		select {
		case header := <-downstreamHeader:
			if !strings.HasPrefix(header, "00-"+incomingTraceID+"-") {
				t.Errorf("downstream traceparent = %q", header)
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

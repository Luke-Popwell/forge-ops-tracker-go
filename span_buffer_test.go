package forgeops

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"
	"time"
)

// spansServer records every trace delivered to /spans (anything else is ignored).
type spansServer struct {
	*httptest.Server
	mu     sync.Mutex
	traces []map[string]any
	paths  []string
}

func newSpansServer(t *testing.T) *spansServer {
	t.Helper()
	s := &spansServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.paths = append(s.paths, r.URL.Path)
		if _, ok := body["spans"]; ok {
			s.traces = append(s.traces, body)
		}
		s.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *spansServer) waitForTraces(t *testing.T, n int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		if len(s.traces) >= n {
			out := append([]map[string]any(nil), s.traces...)
			s.mu.Unlock()
			return out
		}
		s.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d trace(s)", n)
	return nil
}

func (s *spansServer) traceCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.traces)
}

func initTracing(t *testing.T, server *spansServer, configure func(*Configuration)) {
	t.Helper()
	resetForTesting()
	t.Cleanup(resetForTesting)
	Init(func(c *Configuration) {
		c.DSN = "http://key@" + server.Listener.Addr().String() + "/api/v1/events"
		c.Environment = "production"
		c.Release = "1.2.3"
		c.Timeout = time.Second
		c.TraceCaptureThreshold = 0 // send everything unless a test says otherwise
		if configure != nil {
			configure(c)
		}
	})
}

func spansOf(trace map[string]any) []map[string]any {
	var out []map[string]any
	for _, s := range trace["spans"].([]any) {
		out = append(out, s.(map[string]any))
	}
	return out
}

func spanNamed(t *testing.T, trace map[string]any, name string) map[string]any {
	t.Helper()
	for _, s := range spansOf(trace) {
		if s["name"] == name {
			return s
		}
	}
	t.Fatalf("no span named %q in %v", name, trace["spans"])
	return nil
}

func TestWithTraceIsANoOpWhenTracingIsOffOrReportingIsNotEnabled(t *testing.T) {
	server := newSpansServer(t)

	initTracing(t, server, func(c *Configuration) { c.TrackTracing = false })
	if traceFromContext(WithTrace(context.Background())) != nil {
		t.Error("a trace was attached with TrackTracing off")
	}

	initTracing(t, server, func(c *Configuration) { c.Environment = "development" })
	if traceFromContext(WithTrace(context.Background())) != nil {
		t.Error("a trace was attached for an environment reporting is not enabled in")
	}
}

func TestStartSpanAndRecordSpanAreNoOpsWithoutATrace(t *testing.T) {
	ctx, end := StartSpan(context.Background(), "no trace", "service", nil)
	end()
	RecordSpan(ctx, "no trace", "http", time.Now(), time.Millisecond, nil)
	FinishTrace(ctx, "no trace", time.Now(), time.Second) // must not panic or send anything
}

func TestAFinishedTraceIsDeliveredToSpansWithItsNestedTreeAndTheWireShape(t *testing.T) {
	server := newSpansServer(t)
	initTracing(t, server, nil)

	start := time.Now()
	ctx := WithTrace(context.Background())
	chargeCtx, endCharge := StartSpan(ctx, "charge card", "service", map[string]any{"order_id": 42})
	RecordSpan(chargeCtx, "POST payments.example.com", "http", time.Now(), 5*time.Millisecond, map[string]any{"status": 200})
	endCharge()
	RecordSpan(ctx, "SELECT users", "database", time.Now(), time.Millisecond, nil)
	FinishTrace(ctx, "GET /orders/:id", start, 1500*time.Millisecond)

	trace := server.waitForTraces(t, 1)[0]
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(trace["trace_id"].(string)) {
		t.Errorf("trace_id = %v, want 32 hex chars", trace["trace_id"])
	}
	if server.paths[0] != "/api/v1/spans" {
		t.Errorf("delivered to %s, want /api/v1/spans", server.paths[0])
	}

	root := spanNamed(t, trace, "GET /orders/:id")
	charge := spanNamed(t, trace, "charge card")
	call := spanNamed(t, trace, "POST payments.example.com")
	query := spanNamed(t, trace, "SELECT users")

	if root["parent_span_id"] != nil || root["kind"] != "controller" {
		t.Errorf("root = %v, want no parent and kind controller", root)
	}
	if charge["parent_span_id"] != root["span_id"] {
		t.Errorf("charge card parent = %v, want the root %v", charge["parent_span_id"], root["span_id"])
	}
	if call["parent_span_id"] != charge["span_id"] {
		t.Errorf("the http call parent = %v, want charge card %v", call["parent_span_id"], charge["span_id"])
	}
	if query["parent_span_id"] != root["span_id"] {
		t.Errorf("the query recorded after charge card ended has parent %v, want the root", query["parent_span_id"])
	}
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(root["span_id"].(string)) {
		t.Errorf("span_id = %v, want 16 hex chars", root["span_id"])
	}
	if !regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$`).MatchString(root["started_at"].(string)) {
		t.Errorf("started_at = %v, want millisecond-precision UTC", root["started_at"])
	}
	if root["duration_ms"] != float64(1500) || root["environment"] != "production" || root["release"] != "1.2.3" {
		t.Errorf("root = %v, want duration 1500, environment production, release 1.2.3", root)
	}
	if charge["data"].(map[string]any)["order_id"] != float64(42) {
		t.Errorf("charge data = %v", charge["data"])
	}
}

func TestOnlyASlowTraceIsEverSent(t *testing.T) {
	server := newSpansServer(t)
	initTracing(t, server, func(c *Configuration) { c.TraceCaptureThreshold = time.Second })

	fast := WithTrace(context.Background())
	FinishTrace(fast, "GET /fast", time.Now(), 999*time.Millisecond)
	slow := WithTrace(context.Background())
	FinishTrace(slow, "GET /slow", time.Now(), time.Second)

	traces := server.waitForTraces(t, 1)
	time.Sleep(100 * time.Millisecond)
	if server.traceCount() != 1 || spansOf(traces[0])[0]["name"] != "GET /slow" {
		t.Errorf("delivered %d trace(s), want only the one at the threshold", server.traceCount())
	}
}

func TestConcurrentBranchesEachParentCorrectlyNotToEachOther(t *testing.T) {
	// The reason the open span travels in the context rather than on a shared stack: two goroutines
	// each opening a span from the same context must both parent to the root, and a child opened
	// inside one must parent to that one, whatever order they interleave in.
	server := newSpansServer(t)
	initTracing(t, server, nil)
	ctx := WithTrace(context.Background())

	var wg sync.WaitGroup
	for _, name := range []string{"branch a", "branch b"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			branchCtx, end := StartSpan(ctx, name, "service", nil)
			time.Sleep(20 * time.Millisecond)
			RecordSpan(branchCtx, "child of "+name, "database", time.Now(), time.Millisecond, nil)
			end()
		}(name)
	}
	wg.Wait()
	FinishTrace(ctx, "GET /fan-out", time.Now(), time.Second)

	trace := server.waitForTraces(t, 1)[0]
	root := spanNamed(t, trace, "GET /fan-out")
	for _, name := range []string{"branch a", "branch b"} {
		branch := spanNamed(t, trace, name)
		if branch["parent_span_id"] != root["span_id"] {
			t.Errorf("%s parent = %v, want the root", name, branch["parent_span_id"])
		}
		if spanNamed(t, trace, "child of "+name)["parent_span_id"] != branch["span_id"] {
			t.Errorf("child of %s is mis-parented", name)
		}
	}
}

func TestTheEndFuncRecordsOnlyOnceHoweverOftenItIsCalled(t *testing.T) {
	server := newSpansServer(t)
	initTracing(t, server, nil)
	ctx := WithTrace(context.Background())

	_, end := StartSpan(ctx, "once", "service", nil)
	end()
	end()
	FinishTrace(ctx, "root", time.Now(), time.Second)

	spans := spansOf(server.waitForTraces(t, 1)[0])
	if len(spans) != 2 {
		t.Errorf("got %d spans, want 2 (the span once, and the root)", len(spans))
	}
}

func TestTransportRecordsAnHTTPSpanNamedMethodAndHostUnderTheCurrentSpan(t *testing.T) {
	tracker := newSpansServer(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	defer upstream.Close()
	initTracing(t, tracker, nil)
	ctx := WithTrace(context.Background())
	client := &http.Client{Transport: Transport(nil)}

	spanCtx, end := StartSpan(ctx, "call upstream", "service", nil)
	req, _ := http.NewRequestWithContext(spanCtx, http.MethodGet, upstream.URL+"/secret/path?token=abc", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	end()
	FinishTrace(ctx, "GET /x", time.Now(), time.Second)

	trace := tracker.waitForTraces(t, 1)[0]
	call := spanNamed(t, trace, "GET "+upstream.Listener.Addr().String())
	if call["kind"] != "http" || call["data"].(map[string]any)["status"] != float64(http.StatusTeapot) {
		t.Errorf("http span = %v", call)
	}
	if call["parent_span_id"] != spanNamed(t, trace, "call upstream")["span_id"] {
		t.Errorf("http span parent = %v", call["parent_span_id"])
	}
	for _, s := range spansOf(trace) {
		if name, _ := s["name"].(string); regexp.MustCompile(`secret|token`).MatchString(name) {
			t.Errorf("a span name leaked the path or query: %q", name)
		}
	}
}

func TestTransportWithNoTraceInTheRequestContextRecordsNothingAndChangesNothing(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer upstream.Close()
	client := &http.Client{Transport: Transport(nil)}

	resp, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestTransportNeverSwallowsAnErrorAndMarksTheSpan(t *testing.T) {
	tracker := newSpansServer(t)
	initTracing(t, tracker, nil)
	ctx := WithTrace(context.Background())
	client := &http.Client{Transport: Transport(nil), Timeout: 200 * time.Millisecond}

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:1/nothing", nil)
	if _, err := client.Do(req); err == nil {
		t.Fatal("expected a connection error to propagate")
	}
	FinishTrace(ctx, "GET /x", time.Now(), time.Second)

	call := spanNamed(t, tracker.waitForTraces(t, 1)[0], "GET 127.0.0.1:1")
	if call["data"].(map[string]any)["error"] != true {
		t.Errorf("http span data = %v, want error true", call["data"])
	}
}

func TestSpansURISwapsTheTrailingEventsSegment(t *testing.T) {
	c := &Configuration{DSN: "https://key@tracker.example.com/api/v1/events"}
	if got := c.SpansURI(); got != "https://tracker.example.com/api/v1/spans" {
		t.Errorf("SpansURI = %q", got)
	}
	if got := (&Configuration{}).SpansURI(); got != "" {
		t.Errorf("SpansURI with no DSN = %q, want empty", got)
	}
}

func TestASpanQueueThatIsFullDropsTheTraceInsteadOfBlocking(t *testing.T) {
	hold := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-hold; w.WriteHeader(http.StatusOK) }))
	defer slow.Close()
	defer close(hold)
	config := &Configuration{DSN: "http://key@" + slow.Listener.Addr().String() + "/api/v1/events", QueueSize: 1, Timeout: 5 * time.Second, Logger: noopLogger{}}
	queue := NewSpanQueue(config, NewClient(config))

	queue.Push(map[string]any{"trace_id": "a", "spans": []any{}}) // taken by the worker, which then blocks on the slow server
	time.Sleep(100 * time.Millisecond)
	queue.Push(map[string]any{"trace_id": "b", "spans": []any{}}) // fills the one slot

	if queue.Push(map[string]any{"trace_id": "c", "spans": []any{}}) {
		t.Error("a full queue accepted a trace instead of dropping it")
	}
}

func TestNormalizeKindSendsUnknownKindsAsOtherSinceTheServerWouldRejectTheWholeTrace(t *testing.T) {
	for kind, want := range map[string]string{"database": "database", "http": "http", "job": "job", "db": "other", "view": "other", "": "other"} {
		if got := normalizeKind(kind); got != want {
			t.Errorf("normalizeKind(%q) = %q, want %q", kind, got, want)
		}
	}
}

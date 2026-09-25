//go:debug httpmuxgo121=0

// The directive above opts these tests into Go 1.22+'s ServeMux (method and wildcard patterns,
// and http.Request.Pattern), which a module declaring go 1.21 otherwise gets the old behavior of:
// a host app on Go 1.22+ has it by default.

package forgeops

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	incomingTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	incomingSpanID  = "00f067aa0ba902b7"
	incoming        = "00-" + incomingTraceID + "-" + incomingSpanID + "-01"
)

// trackerServer records every event and trace delivered to it.
type trackerServer struct {
	*httptest.Server
	mu     sync.Mutex
	events []map[string]any
	traces []map[string]any
}

func newTrackerServer(t *testing.T) *trackerServer {
	t.Helper()
	s := &trackerServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		if strings.HasSuffix(r.URL.Path, "/spans") {
			s.traces = append(s.traces, body)
		} else if strings.HasSuffix(r.URL.Path, "/events") {
			s.events = append(s.events, body)
		}
		s.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *trackerServer) wait(t *testing.T, what string, get func() int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		n := get()
		s.mu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (s *trackerServer) event(t *testing.T) map[string]any {
	t.Helper()
	s.wait(t, "an event", func() int { return len(s.events) })
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.events[0]
}

func (s *trackerServer) trace(t *testing.T) map[string]any {
	t.Helper()
	s.wait(t, "a trace", func() int { return len(s.traces) })
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.traces[0]
}

func (s *trackerServer) traceCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.traces)
}

func initTracker(t *testing.T, server *trackerServer, configure func(*Configuration)) {
	t.Helper()
	resetForTesting()
	t.Cleanup(resetForTesting)
	Init(func(c *Configuration) {
		c.DetectChanges = false
		c.DSN = "http://key@" + server.Listener.Addr().String() + "/api/v1/events"
		c.Environment = "production"
		c.Timeout = time.Second
		c.Logger = noopLogger{}
		// High enough that only an errored request's trace is sent, unless a test lowers it.
		c.TraceCaptureThreshold = 30 * time.Second
		if configure != nil {
			configure(c)
		}
	})
}

func incomingRequest(method, target, traceparent string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	if traceparent != "" {
		r.Header.Set(TraceParentHeader, traceparent)
	}
	return r
}

func TestParseTraceParentAcceptsValidHeaders(t *testing.T) {
	for _, value := range []string{
		incoming,
		" 00-" + incomingTraceID + "-" + incomingSpanID + "-00 ",
		"cc-" + incomingTraceID + "-" + incomingSpanID + "-01-future-fields",
	} {
		traceID, parentSpanID, ok := parseTraceParent(value)
		if !ok || traceID != incomingTraceID || parentSpanID != incomingSpanID {
			t.Errorf("parseTraceParent(%q) = %q, %q, %v", value, traceID, parentSpanID, ok)
		}
	}
}

func TestParseTraceParentRejectsUnusableHeaders(t *testing.T) {
	for _, value := range []string{
		"",
		"garbage",
		"00-" + incomingTraceID + "-" + incomingSpanID + "-01-extra",
		"ff-" + incomingTraceID + "-" + incomingSpanID + "-01",
		"00-00000000000000000000000000000000-" + incomingSpanID + "-01",
		"00-" + incomingTraceID + "-0000000000000000-01",
		"00-" + strings.ToUpper(incomingTraceID) + "-" + incomingSpanID + "-01",
		"00-" + incomingTraceID + "-" + incomingSpanID,
		"0-" + incomingTraceID + "-" + incomingSpanID + "-01",
	} {
		if _, _, ok := parseTraceParent(value); ok {
			t.Errorf("parseTraceParent(%q) accepted an unusable header", value)
		}
	}
}

func TestBuildAndGeneratedIDsAreW3CShaped(t *testing.T) {
	if got := buildTraceParent(incomingTraceID, incomingSpanID); got != incoming {
		t.Errorf("buildTraceParent = %q", got)
	}
	traceID, spanID := generateTraceID(), generateSpanID()
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(traceID) || !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(spanID) {
		t.Errorf("generated ids %q, %q are not 32/16 lowercase hex", traceID, spanID)
	}
	if traceID == generateTraceID() {
		t.Error("two generated trace ids were equal")
	}
}

func TestPropagatesTracesToEveryHostByDefault(t *testing.T) {
	c := NewConfiguration()
	if !c.PropagateTraces || c.TracePropagationTargets != nil {
		t.Fatalf("defaults = %v, %v; want true, nil", c.PropagateTraces, c.TracePropagationTargets)
	}
	if !c.ShouldPropagateTrace("api.example.com") {
		t.Error("the default should propagate to every host")
	}
	c.PropagateTraces = false
	if c.ShouldPropagateTrace("api.example.com") {
		t.Error("PropagateTraces false still propagated")
	}
}

func TestTracePropagationTargetsMatchHostsOnADotBoundaryAndRegexpsAnywhere(t *testing.T) {
	c := NewConfiguration()
	c.TracePropagationTargets = []any{"Example.com", ".internal.test", regexp.MustCompile(`^10\.0\.`), 42, ""}
	for host, want := range map[string]bool{
		"example.com":           true,
		"API.Example.com":       true,
		"orders.internal.test":  true,
		"10.0.4.2":              true,
		"badexample.com":        false,
		"example.com.evil.test": false,
		"192.10.0.1":            false,
		"":                      false,
	} {
		if got := c.ShouldPropagateTrace(host); got != want {
			t.Errorf("ShouldPropagateTrace(%q) = %v, want %v", host, got, want)
		}
	}

	c.TracePropagationTargets = []any{}
	if c.ShouldPropagateTrace("example.com") {
		t.Error("an empty, non-nil target list should propagate nowhere")
	}
}

func TestWithRequestContinuesAnIncomingTraceOrStartsAFreshOne(t *testing.T) {
	server := newTrackerServer(t)
	initTracker(t, server, nil)

	continued := WithRequest(incomingRequest("GET", "/x", incoming))
	if got := TraceID(continued.Context()); got != incomingTraceID {
		t.Errorf("TraceID = %q, want the incoming %q", got, incomingTraceID)
	}
	again := WithRequest(continued)
	if again != continued || requestFromContext(again.Context()) != requestFromContext(continued.Context()) {
		t.Error("a second WithRequest call should keep the same trace context")
	}

	fresh := WithRequest(incomingRequest("GET", "/x", "00-"+strings.ToUpper(incomingTraceID)+"-"+incomingSpanID+"-01"))
	if got := TraceID(fresh.Context()); !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(got) || got == incomingTraceID {
		t.Errorf("a malformed header should start a fresh trace, got %q", got)
	}

	if TraceID(context.Background()) != "" {
		t.Error("TraceID outside a request or trace should be empty")
	}
}

func TestWithRequestIsANoOpWhenReportingIsNotEnabled(t *testing.T) {
	server := newTrackerServer(t)
	initTracker(t, server, func(c *Configuration) { c.Environment = "development" })

	r := incomingRequest("GET", "/x", incoming)
	if got := WithRequest(r); got != r || TraceID(got.Context()) != "" {
		t.Error("WithRequest attached a trace context for an environment reporting is not enabled in")
	}
}

func TestAnErrorDuringARequestCarriesItsTraceIDTransactionNameAndEndpointAndSendsTheFastTrace(t *testing.T) {
	server := newTrackerServer(t)
	initTracker(t, server, nil)

	r := WithRequest(incomingRequest("POST", "/orders/42", incoming))
	SetRequestRoute(r.Context(), "/orders/:id")
	ctx := WithTrace(r.Context())
	CaptureErrorCtx(ctx, errors.New("checkout failed"), nil, nil)
	FinishTrace(ctx, "POST /orders/:id", time.Now(), time.Millisecond)

	event := server.event(t)
	if event["trace_id"] != incomingTraceID || event["transaction_name"] != "POST /orders/:id" || event["endpoint"] != "POST /orders/:id" {
		t.Errorf("event trace fields = %v, %v, %v", event["trace_id"], event["transaction_name"], event["endpoint"])
	}

	// Far under TraceCaptureThreshold, but the request errored: sent anyway, nested under the caller's span.
	trace := server.trace(t)
	if trace["trace_id"] != incomingTraceID {
		t.Errorf("trace_id = %v, want %v", trace["trace_id"], incomingTraceID)
	}
	if root := spanNamed(t, trace, "POST /orders/:id"); root["parent_span_id"] != incomingSpanID {
		t.Errorf("root parent = %v, want the caller's span %v", root["parent_span_id"], incomingSpanID)
	}
}

func TestAFastRequestWithNoErrorStillSendsNothing(t *testing.T) {
	server := newTrackerServer(t)
	initTracker(t, server, nil)

	ctx := WithTrace(WithRequest(incomingRequest("GET", "/ok", incoming)).Context())
	FinishTrace(ctx, "GET /ok", time.Now(), time.Millisecond)
	time.Sleep(150 * time.Millisecond)
	if n := server.traceCount(); n != 0 {
		t.Errorf("%d trace(s) sent for a fast, successful request", n)
	}
}

func TestTheEndpointComesFromTheServeMuxPatternNeverTheLiteralPath(t *testing.T) {
	server := newTrackerServer(t)
	initTracker(t, server, nil)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		CaptureErrorCtx(r.Context(), errors.New("not found in stock"), nil, nil)
	})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, WithRequest(r))
	})
	handler.ServeHTTP(httptest.NewRecorder(), incomingRequest("GET", "/orders/42", ""))

	event := server.event(t)
	if event["endpoint"] != "GET /orders/{id}" || event["transaction_name"] != "GET /orders/42" {
		t.Errorf("endpoint = %v, transaction_name = %v", event["endpoint"], event["transaction_name"])
	}
}

func TestAnErrorOutsideARequestHasNoTraceFieldsButOneInsideATraceHasItsID(t *testing.T) {
	server := newTrackerServer(t)
	initTracker(t, server, nil)

	CaptureErrorCtx(context.Background(), errors.New("background job"), nil, nil)
	event := server.event(t)
	for _, key := range []string{"trace_id", "transaction_name", "endpoint"} {
		if _, ok := event[key]; ok {
			t.Errorf("an error outside any request carried %q", key)
		}
	}

	ctx := WithTrace(context.Background())
	CaptureErrorCtx(ctx, errors.New("queue job"), nil, nil)
	server.wait(t, "a second event", func() int { return len(server.events) - 1 })
	server.mu.Lock()
	second := server.events[1]
	server.mu.Unlock()
	if second["trace_id"] != TraceID(ctx) || second["endpoint"] != nil {
		t.Errorf("an error inside a WithTrace trace = trace_id %v, endpoint %v", second["trace_id"], second["endpoint"])
	}
}

func TestTransportSendsATraceparentNamingItsOwnRecordedSpan(t *testing.T) {
	server := newTrackerServer(t)
	var received []string
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		received = append(received, r.Header.Get(TraceParentHeader))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	initTracker(t, server, func(c *Configuration) { c.TraceCaptureThreshold = 0 })
	client := &http.Client{Transport: Transport(nil)}

	ctx := WithTrace(WithRequest(incomingRequest("POST", "/checkout", incoming)).Context())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL+"/charge?card=1", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if req.Header.Get(TraceParentHeader) != "" {
		t.Error("Transport modified the caller's own request")
	}

	preset, _ := http.NewRequestWithContext(ctx, http.MethodGet, upstream.URL, nil)
	preset.Header.Set(TraceParentHeader, "00-11111111111111111111111111111111-2222222222222222-01")
	resp, err = client.Do(preset)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	FinishTrace(ctx, "POST /checkout", time.Now(), time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	parts := strings.Split(received[0], "-")
	if len(parts) != 4 || parts[0] != "00" || parts[1] != incomingTraceID || parts[3] != "01" {
		t.Fatalf("traceparent = %q", received[0])
	}
	if received[1] != "00-11111111111111111111111111111111-2222222222222222-01" {
		t.Errorf("a traceparent the app set was replaced with %q", received[1])
	}

	trace := server.trace(t)
	call := spanNamed(t, trace, "POST "+upstream.Listener.Addr().String())
	if call["span_id"] != parts[2] {
		t.Errorf("the header's parent id %q is not the recorded http span's id %v", parts[2], call["span_id"])
	}
	if call["parent_span_id"] != spanNamed(t, trace, "POST /checkout")["span_id"] {
		t.Errorf("http span parent = %v, want the root", call["parent_span_id"])
	}
}

func TestTransportHonorsPropagationSettingsAndNeverTagsItsOwnRequests(t *testing.T) {
	server := newTrackerServer(t)
	var received []string
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		received = append(received, r.Header.Get(TraceParentHeader))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	client := &http.Client{Transport: Transport(nil)}
	get := func(ctx context.Context, url string) {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	last := func() string {
		mu.Lock()
		defer mu.Unlock()
		return received[len(received)-1]
	}

	// Not in the targets ("127.0.0.1" is the upstream's host).
	initTracker(t, server, func(c *Configuration) { c.TracePropagationTargets = []any{"internal.example"} })
	get(WithRequest(incomingRequest("GET", "/x", incoming)).Context(), upstream.URL)
	if got := last(); got != "" {
		t.Errorf("a host outside TracePropagationTargets got %q", got)
	}

	initTracker(t, server, func(c *Configuration) { c.PropagateTraces = false })
	get(WithRequest(incomingRequest("GET", "/x", incoming)).Context(), upstream.URL)
	if got := last(); got != "" {
		t.Errorf("PropagateTraces off still sent %q", got)
	}

	// TrackTracing off: no trace to record a span in, but the request's trace id still goes out.
	initTracker(t, server, func(c *Configuration) { c.TrackTracing = false })
	ctx := WithTrace(WithRequest(incomingRequest("GET", "/x", incoming)).Context())
	get(ctx, upstream.URL)
	if got := last(); !strings.HasPrefix(got, "00-"+incomingTraceID+"-") {
		t.Errorf("with TrackTracing off, traceparent = %q", got)
	}

	// Outside any request or trace: nothing.
	get(context.Background(), upstream.URL)
	if got := last(); got != "" {
		t.Errorf("a request outside any trace got %q", got)
	}

	// This client's own ingestion host: never, even inside a request.
	var ownHeader string
	own := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ownHeader = r.Header.Get(TraceParentHeader)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer own.Close()
	initTracker(t, &trackerServer{Server: own}, nil)
	get(WithRequest(incomingRequest("GET", "/x", incoming)).Context(), own.URL+"/api/v1/spans")
	if ownHeader != "" {
		t.Errorf("this client's own request to ForgeOps got %q", ownHeader)
	}
}

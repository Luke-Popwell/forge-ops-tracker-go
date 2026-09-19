package nethttp

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	forgeops "github.com/Luke-Popwell/forge-ops-tracker-go"
)

var quietLogger = log.New(io.Discard, "", 0)

func TestMiddlewareReportsThenRepanics(t *testing.T) {
	var received int32
	trackerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&received, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer trackerServer.Close()

	forgeops.Init(func(c *forgeops.Configuration) {
		c.DSN = "http://key@" + trackerServer.Listener.Addr().String() + "/events"
		c.Environment = "production"
		c.Timeout = time.Second
	})

	handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))

	appServer := httptest.NewServer(handler)
	appServer.Config.ErrorLog = quietLogger // the panic net/http logs on the way down is expected here, not a real test failure
	defer appServer.Close()

	// net/http.Server recovers a per-request panic itself by closing the connection with no
	// response written at all, rather than crashing the process or sending a 500: so the
	// client-visible effect of Middleware re-panicking (instead of swallowing the panic) is a
	// failed request, not a particular status code. That's the assertion that matters here: the
	// request must fail exactly as it would with no tracker installed at all.
	_, err := http.Get(appServer.URL + "/orders")
	if err == nil {
		t.Fatal("request to the panicking handler unexpectedly succeeded: Recover() may have swallowed the panic")
	}

	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&received) < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&received); got != 1 {
		t.Fatalf("tracker server received %d events, want 1", got)
	}
}

func TestMiddlewarePassesThroughANonPanickingHandler(t *testing.T) {
	handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	server := httptest.NewServer(handler)
	defer server.Close()

	resp, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusTeapot)
	}
}

// Both breadcrumb scenarios (Timing's own automatic entry, and a manually-added one) share a
// single trackerServer and a single forgeops.Init call, rather than each getting its own: a
// second Init call, changing Configuration fields a still-running background goroutine from an
// earlier test might concurrently be reading, is a real, confirmed data race in this SDK's own
// package-level singleton (go test -race catches it directly: PerformanceFlusher.Flush reads
// Configuration fields with no synchronization at all against Init's own unsynchronized writes to
// those same fields). That's a genuine, pre-existing gap in Configuration's own concurrency model,
// not something breadcrumbs caused, and fixing it properly means synchronizing every background
// goroutine's Configuration field access consistently (DeliveryQueue, Reporter, EventBuilder,
// Client, PerformanceFlusher alike), a real, separate piece of work belonging to its own pass, not
// a side effect of adding tests for a different feature. Avoided here, honestly, rather than
// silently relied on to not flake: one Init, one tracker destination, two sequential requests
// against two different app servers pointed at the very same tracker.
// Every scenario below that needs Timing (and so triggers RecordPerformance, and so starts
// forgeops' own package-level PerformanceFlusher's background goroutine) shares this single
// trackerServer and single forgeops.Init call, deliberately, rather than each getting its own:
// calling Init a second time, changing Configuration fields a still-running background goroutine
// from an earlier call might concurrently be reading, is a real, confirmed data race in this SDK's
// own package-level singleton (go test -race catches it directly: PerformanceFlusher.Flush reads
// Configuration fields with no synchronization at all against Init's own unsynchronized writes to
// those same fields, first surfaced while developing this exact test file). That's a genuine,
// pre-existing gap in Configuration's own concurrency model, not something breadcrumbs caused, and
// fixing it properly means synchronizing every background goroutine's Configuration field access
// consistently (DeliveryQueue, Reporter, EventBuilder, Client, PerformanceFlusher alike): real,
// separate work belonging to its own dedicated pass, not a side effect of adding tests for a
// different feature. Contained here, honestly, rather than silently relied on to not flake: one
// Init, one tracker destination, for every test in this file that needs Timing at all.
func TestBreadcrumbsAndTiming(t *testing.T) {
	eventCh := make(chan map[string]any, 2)
	var samplesMu sync.Mutex
	// Every transaction_name ever seen across every flush, not just the latest: a flush batches
	// every bucket accumulated since the last one (see PerformanceFlusher.Flush), so two subtests
	// below whose own requests happen to land in the same 20ms window arrive together, in a
	// randomized order (Go's own map iteration order, which Flush ranges over directly with no
	// sorting). Reading only samples[0] here (this test's own first draft) intermittently missed
	// whichever transaction didn't happen to land first in that batch: a real, reproducible flake
	// this exact rollout's own -race runs caught directly, not a hypothetical.
	seenTransactionNames := map[string]bool{}
	trackerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		// A real event delivery (a message field on it) goes to eventCh; a performance samples
		// batch (a "samples" field instead, never "message") updates seenTransactionNames instead:
		// the two are structurally distinguishable on sight, not by which endpoint path they
		// happened to hit, since both land on this same tracker's single handler.
		if _, ok := body["message"]; ok {
			eventCh <- body
		}
		if samples, ok := body["samples"].([]any); ok {
			samplesMu.Lock()
			for _, sample := range samples {
				name, _ := sample.(map[string]any)["transaction_name"].(string)
				if name != "" {
					seenTransactionNames[name] = true
				}
			}
			samplesMu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer trackerServer.Close()

	forgeops.Init(func(c *forgeops.Configuration) {
		c.DSN = "http://key@" + trackerServer.Listener.Addr().String() + "/events"
		c.Environment = "production"
		c.Timeout = time.Second
		c.PerformanceFlushInterval = 20 * time.Millisecond
	})

	t.Run("Timing's own automatic controller breadcrumb", func(t *testing.T) {
		// Middleware(Timing(...)), not the other way around: Middleware is what actually calls
		// forgeops.WithBreadcrumbs, so it has to be the outer layer for Timing's own
		// AddBreadcrumb call to find a trail to add to at all.
		handler := Middleware(Timing(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			panic("boom")
		})))
		appServer := httptest.NewServer(handler)
		appServer.Config.ErrorLog = quietLogger
		defer appServer.Close()

		_, _ = http.Get(appServer.URL + "/orders/42")

		select {
		case event := <-eventCh:
			crumbs, _ := event["breadcrumbs"].([]any)
			if len(crumbs) != 1 {
				t.Fatalf("breadcrumbs = %+v, want exactly one automatic controller entry", event["breadcrumbs"])
			}
			crumb, _ := crumbs[0].(map[string]any)
			if crumb["category"] != "controller" || crumb["message"] != "GET /orders/42" {
				t.Errorf("breadcrumb = %+v, want category controller, message %q", crumb, "GET /orders/42")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("tracker server never received the reported event")
		}
	})

	t.Run("a manually-added breadcrumb", func(t *testing.T) {
		handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			forgeops.AddBreadcrumb(r.Context(), "charged card", "custom", "info", map[string]any{"order_id": 42})
			panic("boom")
		}))
		appServer := httptest.NewServer(handler)
		appServer.Config.ErrorLog = quietLogger
		defer appServer.Close()

		_, _ = http.Get(appServer.URL + "/checkout")

		select {
		case event := <-eventCh:
			crumbs, _ := event["breadcrumbs"].([]any)
			if len(crumbs) != 1 {
				t.Fatalf("breadcrumbs = %+v, want exactly the one manually-added entry", event["breadcrumbs"])
			}
			crumb, _ := crumbs[0].(map[string]any)
			if crumb["message"] != "charged card" {
				t.Errorf("breadcrumb message = %v, want %q", crumb["message"], "charged card")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("tracker server never received the reported event")
		}
	})

	t.Run("Timing reports the request's own duration by path", func(t *testing.T) {
		handler := Timing(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		appServer := httptest.NewServer(handler)
		defer appServer.Close()

		_, err := http.Get(appServer.URL + "/users/42")
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}

		deadline := time.Now().Add(2 * time.Second)
		for {
			samplesMu.Lock()
			seen := seenTransactionNames["GET /users/42"]
			samplesMu.Unlock()
			if seen {
				// The literal path, not a matched route pattern: plain net/http has no
				// route-matching concept of its own (see Timing's own doc comment on why).
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("performance sample for GET /users/42 was not delivered in time")
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
}

func TestTimingOpensATraceAndSendsASlowRequestsSpans(t *testing.T) {
	traceCh := make(chan map[string]any, 2)
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, ok := body["trace_id"]; ok {
			traceCh <- body
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer tracker.Close()

	forgeops.Init(func(c *forgeops.Configuration) {
		c.DSN = "http://key@" + tracker.Listener.Addr().String() + "/api/v1/events"
		c.Environment = "production"
		c.Timeout = time.Second
		c.TrackTracing = true
		c.TraceCaptureThreshold = 40 * time.Millisecond
	})

	handler := Timing(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			_, end := forgeops.StartSpan(r.Context(), "charge card", "service", nil)
			time.Sleep(60 * time.Millisecond)
			end()
		}
		w.WriteHeader(http.StatusOK)
	}))
	app := httptest.NewServer(handler)
	defer app.Close()

	if resp, err := http.Get(app.URL + "/fast"); err == nil {
		resp.Body.Close()
	}
	if resp, err := http.Get(app.URL + "/slow"); err == nil {
		resp.Body.Close()
	}

	select {
	case trace := <-traceCh:
		names := map[string]map[string]any{}
		for _, s := range trace["spans"].([]any) {
			span := s.(map[string]any)
			names[span["name"].(string)] = span
		}
		root := names["GET /slow"]
		charge := names["charge card"]
		if root == nil || charge == nil {
			t.Fatalf("spans = %v, want a root GET /slow and a charge card span", trace["spans"])
		}
		if root["parent_span_id"] != nil || charge["parent_span_id"] != root["span_id"] {
			t.Errorf("root = %v, charge = %v: charge card should nest under the root", root, charge)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the slow request's trace was never delivered")
	}

	select {
	case extra := <-traceCh:
		t.Errorf("a second trace was delivered (%v): the fast request must never be sent", extra["spans"])
	case <-time.After(150 * time.Millisecond):
	}
}

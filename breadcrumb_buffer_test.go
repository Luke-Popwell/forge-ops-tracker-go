package forgeops

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestBreadcrumbBufferAddPreservesOrderAndDefaults(t *testing.T) {
	buf := newBreadcrumbBuffer(30)
	buf.add("first", "", "", nil)
	buf.add("second", "custom", "warning", map[string]any{"n": 1})

	entries := buf.all()
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2", len(entries))
	}
	if entries[0].Message != "first" || entries[0].Category != "custom" || entries[0].Level != "info" {
		t.Errorf("entries[0] = %+v, want defaulted category/level", entries[0])
	}
	if entries[0].Data == nil {
		t.Errorf("entries[0].Data = nil, want an empty map, not nil")
	}
	if entries[1].Message != "second" || entries[1].Level != "warning" || entries[1].Data["n"] != 1 {
		t.Errorf("entries[1] = %+v", entries[1])
	}
	if entries[0].Timestamp == "" {
		t.Errorf("entries[0].Timestamp is empty")
	}
}

func TestBreadcrumbBufferDropsOldestOnceOverMaxSize(t *testing.T) {
	buf := newBreadcrumbBuffer(2)
	buf.add("one", "", "", nil)
	buf.add("two", "", "", nil)
	buf.add("three", "", "", nil)

	entries := buf.all()
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2", len(entries))
	}
	if entries[0].Message != "two" || entries[1].Message != "three" {
		t.Errorf("entries = %+v, want [two three] (oldest dropped)", entries)
	}
}

func TestBreadcrumbBufferMaxSizeZeroKeepsNothing(t *testing.T) {
	buf := newBreadcrumbBuffer(0)
	buf.add("one", "", "", nil)

	if entries := buf.all(); len(entries) != 0 {
		t.Errorf("len(entries) = %d, want 0 for a zero max size", len(entries))
	}
}

// Run with -race: a real concurrency guarantee, not assumed from the mutex merely being present.
func TestBreadcrumbBufferAddIsSafeForConcurrentUse(t *testing.T) {
	buf := newBreadcrumbBuffer(1000)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			buf.add("crumb", "custom", "info", map[string]any{"n": n})
		}(i)
	}
	wg.Wait()

	if got := len(buf.all()); got != 50 {
		t.Errorf("len(entries) = %d, want 50", got)
	}
}

func TestWithBreadcrumbsAttachesAnEmptyTrail(t *testing.T) {
	resetForTesting()
	t.Cleanup(resetForTesting)
	Init(func(c *Configuration) {
		c.DetectChanges = false
		c.TrackBreadcrumbs = true
		c.DSN = "https://key@forgeops.example/events"
		c.Environment = "production"
	})

	ctx := WithBreadcrumbs(context.Background())
	if got := breadcrumbsFromContext(ctx); len(got) != 0 {
		t.Errorf("breadcrumbsFromContext = %+v, want empty for a brand-new trail", got)
	}

	AddBreadcrumb(ctx, "hello", "custom", "info", nil)
	if got := breadcrumbsFromContext(ctx); len(got) != 1 || got[0].Message != "hello" {
		t.Errorf("breadcrumbsFromContext = %+v, want one entry", got)
	}
}

func TestWithBreadcrumbsIsANoOpWhenTrackingIsOff(t *testing.T) {
	resetForTesting()
	t.Cleanup(resetForTesting)
	Init(func(c *Configuration) {
		c.DetectChanges = false
		c.TrackBreadcrumbs = false
		c.DSN = "https://key@forgeops.example/events"
		c.Environment = "production"
	})

	ctx := WithBreadcrumbs(context.Background())
	AddBreadcrumb(ctx, "hello", "", "", nil)

	if got := breadcrumbsFromContext(ctx); got != nil {
		t.Errorf("breadcrumbsFromContext = %+v, want nil when TrackBreadcrumbs is false", got)
	}
}

func TestAddBreadcrumbIsANoOpWithoutWithBreadcrumbs(t *testing.T) {
	// No panic, no error, just nothing recorded: the documented behavior for a context nobody
	// ever called WithBreadcrumbs on.
	AddBreadcrumb(context.Background(), "hello", "", "", nil)

	if got := breadcrumbsFromContext(context.Background()); got != nil {
		t.Errorf("breadcrumbsFromContext = %+v, want nil", got)
	}
}

func TestCaptureErrorCtxDeliversTheAccumulatedTrail(t *testing.T) {
	// A buffered channel handoff, not a bare `var body []byte` read in a spin-loop: see
	// reporter_test.go's TestReporterReportIncludesTheGivenUserInTheDeliveredPayload for the real,
	// go test -race-confirmed data race that exact shape had elsewhere in this same package.
	bodyCh := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodyCh <- body
		w.WriteHeader(http.StatusOK)
	}))
	resetForTesting()
	t.Cleanup(func() {
		server.Close()
		resetForTesting()
	})
	Init(func(c *Configuration) {
		c.DetectChanges = false
		c.DSN = "http://key@" + server.Listener.Addr().String() + "/events"
		c.Environment = "production"
		c.Timeout = time.Second
		c.Logger = noopLogger{}
	})

	ctx := WithBreadcrumbs(context.Background())
	AddBreadcrumb(ctx, "GET /orders/42", "controller", "info", map[string]any{"status": 200})
	CaptureErrorCtx(ctx, errors.New("boom"), nil, nil)

	var body []byte
	select {
	case body = <-bodyCh:
	case <-time.After(2 * time.Second):
		t.Fatal("server never received a request")
	}

	var decoded struct {
		Breadcrumbs []Breadcrumb `json:"breadcrumbs"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v (body: %s)", err, body)
	}
	if len(decoded.Breadcrumbs) != 1 || decoded.Breadcrumbs[0].Message != "GET /orders/42" {
		t.Errorf("delivered breadcrumbs = %+v, want the one entry added before reporting", decoded.Breadcrumbs)
	}
}

func TestCaptureErrorWithoutCtxNeverAttachesBreadcrumbs(t *testing.T) {
	// A buffered channel handoff, not a spin-loop: see TestCaptureErrorCtxDeliversTheAccumulatedTrail
	// above for why.
	bodyCh := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodyCh <- body
		w.WriteHeader(http.StatusOK)
	}))
	resetForTesting()
	t.Cleanup(func() {
		server.Close()
		resetForTesting()
	})
	Init(func(c *Configuration) {
		c.DetectChanges = false
		c.DSN = "http://key@" + server.Listener.Addr().String() + "/events"
		c.Environment = "production"
		c.Timeout = time.Second
		c.Logger = noopLogger{}
	})

	// The plain, pre-existing CaptureError: never carried a ctx before this feature existed, and
	// still doesn't, so it should never send a "breadcrumbs" key at all, not an empty one.
	CaptureError(errors.New("boom"), nil, nil)

	var body []byte
	select {
	case body = <-bodyCh:
	case <-time.After(2 * time.Second):
		t.Fatal("server never received a request")
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v (body: %s)", err, body)
	}
	if _, ok := decoded["breadcrumbs"]; ok {
		t.Errorf("delivered payload has a breadcrumbs key, want it omitted entirely")
	}
}

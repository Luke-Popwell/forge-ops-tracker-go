package forgeops

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// resetAndInit resets forgeops' package-level state, points it at a fresh httptest.Server, and
// returns a counter of how many events that server has received: letting each test below drive
// the real Init/CaptureError/Recover public API end-to-end rather than the lower-level types
// directly, the same "exercise the actual entry points" coverage forgeops_test.go's counterparts
// have in every other client in this repo.
func resetAndInit(t *testing.T) *int32 {
	t.Helper()
	var received int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&received, 1)
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
	return &received
}

func TestInitAppliesConfiguration(t *testing.T) {
	resetForTesting()
	t.Cleanup(resetForTesting)

	config := Init(func(c *Configuration) {
		c.DetectChanges = false
		c.DSN = "https://key@forgeops.example/events"
		c.Release = "abc123"
	})

	if config.DSN != "https://key@forgeops.example/events" {
		t.Errorf("DSN = %q", config.DSN)
	}
	if config.Release != "abc123" {
		t.Errorf("Release = %q", config.Release)
	}
}

func TestInitReturnsTheSameConfigurationOnRepeatedCalls(t *testing.T) {
	resetForTesting()
	t.Cleanup(resetForTesting)

	first := Init(func(c *Configuration) { c.Release = "v1" })
	second := Init(func(c *Configuration) { c.Environment = "production" })

	if first != second {
		t.Error("Init() returned a different *Configuration on the second call")
	}
	if second.Release != "v1" {
		t.Error("second Init() call lost the first call's configuration")
	}
}

func TestCaptureErrorDeliversThroughTheFullStack(t *testing.T) {
	received := resetAndInit(t)

	CaptureError(errors.New("boom"), map[string]any{"order_id": 7}, nil)

	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(received) < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt32(received); got != 1 {
		t.Fatalf("server received %d requests, want 1", got)
	}
}

func TestCaptureErrorIgnoresNilError(t *testing.T) {
	received := resetAndInit(t)

	CaptureError(nil, nil, nil)
	time.Sleep(50 * time.Millisecond)

	if got := atomic.LoadInt32(received); got != 0 {
		t.Errorf("server received %d requests, want 0 for a nil error", got)
	}
}

func TestCaptureErrorIncludesTheGivenUserInTheDeliveredPayload(t *testing.T) {
	// A buffered channel handoff, not a bare `var body []byte` read in a spin-loop: see
	// reporter_test.go's identical fix for the real, go test -race-confirmed data race this
	// same shape had (the delivery queue's own background goroutine writes body, this test's own
	// goroutine was reading it with no synchronization at all).
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

	CaptureError(errors.New("boom"), nil, map[string]any{"id": 42, "email": "alice@example.com"})

	select {
	case body := <-bodyCh:
		if !strings.Contains(string(body), "alice@example.com") {
			t.Fatalf("delivered body = %s, want it to contain the given user's email", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server never received a request")
	}
}

func TestRecoverReportsThenRepanics(t *testing.T) {
	received := resetAndInit(t)

	panicked := false
	func() {
		defer func() {
			if recover() != nil {
				panicked = true
			}
		}()
		defer Recover(nil, nil)
		panic("boom")
	}()

	if !panicked {
		t.Error("Recover() swallowed the panic instead of re-panicking")
	}

	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(received) < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt32(received); got != 1 {
		t.Fatalf("server received %d requests, want 1", got)
	}
}

// initAgainst resets package-level state and points Init at server, the setup resetAndInit does
// for its own server.
func initAgainst(t *testing.T, server *httptest.Server, path string, timeout time.Duration) {
	t.Helper()
	resetForTesting()
	t.Cleanup(func() {
		server.Close()
		resetForTesting()
	})
	Init(func(c *Configuration) {
		c.DetectChanges = false
		c.DSN = "http://key@" + server.Listener.Addr().String() + path
		c.Environment = "production"
		c.Timeout = timeout
		c.Logger = noopLogger{}
	})
}

// A server slow enough that, without Recover's own flush, the panic would be caught below well
// before its report arrived.
func TestRecoverWaitsForDeliveryBeforeRepanicking(t *testing.T) {
	var received int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		atomic.AddInt32(&received, 1)
		w.WriteHeader(http.StatusOK)
	}))
	initAgainst(t, server, "/events", time.Second)

	deliveredWhenRepanicked := int32(-1)
	func() {
		defer func() {
			if recover() != nil {
				deliveredWhenRepanicked = atomic.LoadInt32(&received)
			}
		}()
		defer Recover(nil, nil)
		panic("boom")
	}()

	if deliveredWhenRepanicked != 1 {
		t.Fatalf("server had received %d requests when the panic resumed, want 1", deliveredWhenRepanicked)
	}
}

func TestFlushDeliversQueuedErrorsAndBufferedMetrics(t *testing.T) {
	var events, metrics int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(30 * time.Millisecond)
		if strings.HasSuffix(r.URL.Path, "/events") {
			atomic.AddInt32(&events, 1)
		} else {
			atomic.AddInt32(&metrics, 1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	initAgainst(t, server, "/api/v1/events", time.Second)

	CaptureError(errors.New("first"), nil, nil)
	CaptureError(errors.New("second"), nil, nil)
	CaptureMetric("signup", 1)

	if !Flush(2 * time.Second) {
		t.Fatal("Flush() = false, want true")
	}
	if got := atomic.LoadInt32(&events); got != 2 {
		t.Errorf("server had received %d events when Flush returned, want 2", got)
	}
	if got := atomic.LoadInt32(&metrics); got != 1 {
		t.Errorf("server had received %d metric batches when Flush returned, want 1", got)
	}
}

func TestFlushReturnsFalseWhenDeliveryOutlastsTheTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	initAgainst(t, server, "/events", 5*time.Second)
	// Registered after initAgainst so it runs first: server.Close waits for the blocked handler.
	t.Cleanup(func() { close(release) })

	CaptureError(errors.New("boom"), nil, nil)

	started := time.Now()
	if Flush(100 * time.Millisecond) {
		t.Fatal("Flush() = true, want false: the delivery was still blocked on the server")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("Flush(100ms) returned after %v", elapsed)
	}
}

func TestFlushBeforeAnythingWasReportedReturnsTrue(t *testing.T) {
	resetForTesting()
	t.Cleanup(resetForTesting)

	if !Flush(time.Second) {
		t.Error("Flush() with nothing reported = false, want true")
	}
}

func TestRecoverDoesNothingWithoutAPanic(t *testing.T) {
	received := resetAndInit(t)

	func() {
		defer Recover(nil, nil)
	}()
	time.Sleep(50 * time.Millisecond)

	if got := atomic.LoadInt32(received); got != 0 {
		t.Errorf("server received %d requests, want 0 when there was no panic", got)
	}
}

func TestPanicErrorPassesThroughAnExistingError(t *testing.T) {
	original := errors.New("boom")
	if got := panicError(original); got != original {
		t.Errorf("panicError() wrapped an existing error instead of passing it through")
	}
}

func TestPanicErrorWrapsANonErrorValue(t *testing.T) {
	err := panicError("boom")
	if err.Error() != "boom" {
		t.Errorf("panicError(\"boom\").Error() = %q, want %q", err.Error(), "boom")
	}
}

func TestExportedPanicErrorMatchesTheInternalConversion(t *testing.T) {
	if PanicError("boom").Error() != panicError("boom").Error() {
		t.Error("PanicError() should be the same conversion panicError() does internally")
	}
}

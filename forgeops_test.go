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

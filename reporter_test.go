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

func TestReporterReportSkipsWhenDisabled(t *testing.T) {
	var received int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&received, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// No DSN configured: IsEnabled() is false, so Report must never even reach the queue.
	config := &Configuration{Environment: "production", EnabledEnvironments: map[string]bool{"production": true}, Logger: noopLogger{}}
	reporter := NewReporter(config, NewEventBuilder(config), NewDeliveryQueue(config, NewClient(config)))

	reporter.Report(errors.New("boom"), nil, nil, capturePcs(), nil)
	time.Sleep(50 * time.Millisecond)

	if got := atomic.LoadInt32(&received); got != 0 {
		t.Errorf("server received %d requests, want 0 (reporting should have been skipped)", got)
	}
}

func TestReporterReportDeliversWhenEnabled(t *testing.T) {
	var received int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&received, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	config := &Configuration{
		DSN:                 "http://key@" + server.Listener.Addr().String() + "/events",
		Environment:         "production",
		EnabledEnvironments: map[string]bool{"production": true},
		Timeout:             time.Second,
		Logger:              noopLogger{},
	}
	reporter := NewReporter(config, NewEventBuilder(config), NewDeliveryQueue(config, NewClient(config)))

	reporter.Report(errors.New("boom"), nil, nil, capturePcs(), nil)

	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&received) < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&received); got != 1 {
		t.Fatalf("server received %d requests, want 1", got)
	}
}

func TestReporterReportIncludesTheGivenUserInTheDeliveredPayload(t *testing.T) {
	// A buffered channel handoff, not a bare `var body []byte` read in a spin-loop: the delivery
	// queue's own background goroutine is what actually calls the handler below, so the previous
	// spin-loop's unsynchronized read of body from the test's own goroutine was a real, confirmed
	// data race (caught by `go test -race`, not assumed from the shape alone), racing that
	// goroutine's own unsynchronized write to the same variable.
	bodyCh := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodyCh <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	config := &Configuration{
		DSN:                 "http://key@" + server.Listener.Addr().String() + "/events",
		Environment:         "production",
		EnabledEnvironments: map[string]bool{"production": true},
		Timeout:             time.Second,
		Logger:              noopLogger{},
	}
	reporter := NewReporter(config, NewEventBuilder(config), NewDeliveryQueue(config, NewClient(config)))

	reporter.Report(errors.New("boom"), nil, map[string]any{"id": 42, "email": "alice@example.com"}, capturePcs(), nil)

	select {
	case body := <-bodyCh:
		if !strings.Contains(string(body), "alice@example.com") {
			t.Fatalf("delivered body = %s, want it to contain the given user's email", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server never received a request")
	}
}

func TestReporterReportNeverPanicsOnNilError(t *testing.T) {
	config := &Configuration{Environment: "production", EnabledEnvironments: map[string]bool{"production": true}, Logger: noopLogger{}}
	reporter := NewReporter(config, NewEventBuilder(config), NewDeliveryQueue(config, NewClient(config)))

	// Must not panic: an error reporter that can crash the host app while reporting is the
	// worst possible failure mode.
	reporter.Report(nil, nil, nil, capturePcs(), nil)
}

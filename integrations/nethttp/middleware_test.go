package nethttp

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
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
	// response written at all, rather than crashing the process or sending a 500 -- so the
	// client-visible effect of Middleware re-panicking (instead of swallowing the panic) is a
	// failed request, not a particular status code. That's the assertion that matters here: the
	// request must fail exactly as it would with no tracker installed at all.
	_, err := http.Get(appServer.URL + "/orders")
	if err == nil {
		t.Fatal("request to the panicking handler unexpectedly succeeded -- Recover() may have swallowed the panic")
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

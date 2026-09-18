package forgeops

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newPerformanceConfiguration(dsn string) *Configuration {
	return &Configuration{DSN: dsn, Release: "1.2.3", Environment: "production", Timeout: time.Second, Logger: noopLogger{}}
}

func TestPerformanceFlusherBucketsByTransactionNameAndDeliversOnFlush(t *testing.T) {
	var mu sync.Mutex
	var delivered []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		samples, _ := body["samples"].([]any)
		for _, s := range samples {
			delivered = append(delivered, s.(map[string]any))
		}
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	config := newPerformanceConfiguration("http://key@" + server.Listener.Addr().String() + "/api/v1/events")
	flusher := NewPerformanceFlusher(config, NewClient(config))

	flusher.Record("GET /posts/:id", 100)
	flusher.Record("GET /posts/:id", 200)
	flusher.Record("GET /posts", 50)
	flusher.Flush()

	mu.Lock()
	defer mu.Unlock()
	if len(delivered) != 2 {
		t.Fatalf("delivered %d samples, want 2 (one per distinct transaction)", len(delivered))
	}

	var show, index map[string]any
	for _, s := range delivered {
		switch s["transaction_name"] {
		case "GET /posts/:id":
			show = s
		case "GET /posts":
			index = s
		}
	}
	if show == nil || index == nil {
		t.Fatal("expected both transactions represented")
	}
	if show["request_count"] != float64(2) {
		t.Errorf("show request_count = %v, want 2", show["request_count"])
	}
	if show["duration_sum_ms"] != float64(300) {
		t.Errorf("show duration_sum_ms = %v, want 300", show["duration_sum_ms"])
	}
	if show["max_duration_ms"] != float64(200) {
		t.Errorf("show max_duration_ms = %v, want 200", show["max_duration_ms"])
	}
	if show["release"] != "1.2.3" || show["environment"] != "production" {
		t.Errorf("release/environment not carried through: %v/%v", show["release"], show["environment"])
	}
}

func TestPerformanceFlusherDoesNothingOnAFlushWithNothingRecorded(t *testing.T) {
	var called int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&called, 1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	config := newPerformanceConfiguration("http://key@" + server.Listener.Addr().String() + "/api/v1/events")
	flusher := NewPerformanceFlusher(config, NewClient(config))

	flusher.Flush()

	if atomic.LoadInt32(&called) != 0 {
		t.Error("expected no delivery attempt for an empty flush")
	}
}

func TestPerformanceFlusherKeepsTheBucketForTheNextFlushWhenDeliveryFails(t *testing.T) {
	var shouldSucceed atomic.Bool
	var mu sync.Mutex
	var lastCount float64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !shouldSucceed.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		samples, _ := body["samples"].([]any)
		mu.Lock()
		lastCount = samples[0].(map[string]any)["request_count"].(float64)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	config := newPerformanceConfiguration("http://key@" + server.Listener.Addr().String() + "/api/v1/events")
	flusher := NewPerformanceFlusher(config, NewClient(config))

	flusher.Record("GET /posts/:id", 100)
	flusher.Flush() // fails; the bucket must not be reset

	shouldSucceed.Store(true)
	flusher.Record("GET /posts/:id", 100)
	flusher.Flush()

	mu.Lock()
	defer mu.Unlock()
	if lastCount != 2 {
		t.Errorf("request_count on the successful flush = %v, want 2 (the failed flush's bucket must have survived)", lastCount)
	}
}

// A deterministic reproduction of a real, confirmed bug: a Record call landing in the real window
// Flush's own delivery leaves unlocked (between snapshotting a bucket and that delivery actually
// succeeding) used to be silently destroyed, once Flush got around to resetting, even though it
// was never part of what actually got delivered. Deterministic, not timing-dependent, unlike the
// integration tests in integrations/nethttp and integrations/gin that first caught this: the
// tracker server here blocks mid-delivery on a real channel handshake, so the concurrent Record
// call below is guaranteed to land inside the exact race window on every run, not just sometimes.
func TestPerformanceFlusherNeverLosesARecordThatArrivesDuringDelivery(t *testing.T) {
	deliveryStarted := make(chan struct{})
	releaseDelivery := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(deliveryStarted)
		<-releaseDelivery
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	config := newPerformanceConfiguration("http://key@" + server.Listener.Addr().String() + "/api/v1/events")
	flusher := NewPerformanceFlusher(config, NewClient(config))

	flusher.Record("GET /posts/:id", 100)

	flushDone := make(chan struct{})
	go func() {
		flusher.Flush()
		close(flushDone)
	}()

	<-deliveryStarted // the first flush is now mid-delivery, holding no lock
	// Lands squarely in the race window: a second request for the exact same transaction the
	// in-flight delivery already snapshotted, plus a brand-new one it never saw at all.
	flusher.Record("GET /posts/:id", 300)
	flusher.Record("GET /comments", 40)
	close(releaseDelivery)
	<-flushDone

	var mu sync.Mutex
	var delivered []map[string]any
	server2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		samples, _ := body["samples"].([]any)
		for _, s := range samples {
			delivered = append(delivered, s.(map[string]any))
		}
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server2.Close()
	config.DSN = "http://key@" + server2.Listener.Addr().String() + "/api/v1/events"

	flusher.Flush()

	mu.Lock()
	defer mu.Unlock()
	// Decoded through encoding/json into interface{}, same as every other test in this file that
	// reads a delivered payload back: a JSON number always comes back as float64, never int64.
	var postsCount, commentsCount float64
	for _, s := range delivered {
		switch s["transaction_name"] {
		case "GET /posts/:id":
			postsCount = s["request_count"].(float64)
		case "GET /comments":
			commentsCount = s["request_count"].(float64)
		}
	}
	if postsCount != 1 {
		t.Errorf("GET /posts/:id request_count on the next flush = %v, want 1 (the concurrent Record, on top of the one already delivered)", postsCount)
	}
	if commentsCount != 1 {
		t.Errorf("GET /comments request_count on the next flush = %v, want 1 (a brand-new bucket the in-flight delivery never saw at all)", commentsCount)
	}
}

func TestPerformanceFlusherTickerFlushesOnItsOwn(t *testing.T) {
	delivered := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		select {
		case delivered <- struct{}{}:
		default:
		}
	}))
	defer server.Close()

	config := newPerformanceConfiguration("http://key@" + server.Listener.Addr().String() + "/api/v1/events")
	config.PerformanceFlushInterval = 20 * time.Millisecond
	flusher := NewPerformanceFlusher(config, NewClient(config))

	flusher.Record("GET /posts/:id", 100)

	select {
	case <-delivered:
	case <-time.After(2 * time.Second):
		t.Fatal("performance sample was not delivered by the ticker in time")
	}
}

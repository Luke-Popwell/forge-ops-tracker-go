package forgeops

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestDeliveryQueuePushDeliversInBackground(t *testing.T) {
	var received int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&received, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	config := &Configuration{DSN: "http://key@" + server.Listener.Addr().String() + "/events", QueueSize: 10, Timeout: time.Second, Logger: noopLogger{}}
	queue := NewDeliveryQueue(config, NewClient(config))

	queue.Push(map[string]any{"n": 1})
	queue.Push(map[string]any{"n": 2})

	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&received) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	if got := atomic.LoadInt32(&received); got != 2 {
		t.Fatalf("server received %d deliveries, want 2", got)
	}
}

func TestDeliveryQueueDropsWhenFull(t *testing.T) {
	// A handler that blocks until the test releases it, so the worker goroutine stays busy on the
	// first delivery long enough for the queue behind it to actually fill up.
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	defer close(release)

	config := &Configuration{DSN: "http://key@" + server.Listener.Addr().String() + "/events", QueueSize: 1, Timeout: 5 * time.Second, Logger: noopLogger{}}
	queue := NewDeliveryQueue(config, NewClient(config))

	if ok := queue.Push(map[string]any{"n": 1}); !ok {
		t.Fatal("first push should have succeeded")
	}
	time.Sleep(50 * time.Millisecond) // let the worker pick it up and block inside the handler

	if ok := queue.Push(map[string]any{"n": 2}); !ok {
		t.Fatal("second push should have filled the size-1 queue, not been dropped itself")
	}
	if ok := queue.Push(map[string]any{"n": 3}); ok {
		t.Error("third push should have been dropped: queue was full and the worker still busy")
	}
}

func TestDeliveryQueueFlushWaitsForEverythingQueued(t *testing.T) {
	// Slow enough that nothing would have arrived yet if Flush returned without waiting.
	var received int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(30 * time.Millisecond)
		atomic.AddInt32(&received, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	config := &Configuration{DSN: "http://key@" + server.Listener.Addr().String() + "/events", QueueSize: 10, Timeout: time.Second, Logger: noopLogger{}}
	queue := NewDeliveryQueue(config, NewClient(config))

	for n := 1; n <= 3; n++ {
		queue.Push(map[string]any{"n": n})
	}

	if !queue.Flush(2 * time.Second) {
		t.Fatal("Flush() = false, want true: three quick deliveries fit well inside the timeout")
	}
	if got := atomic.LoadInt32(&received); got != 3 {
		t.Fatalf("server had received %d deliveries when Flush returned, want 3", got)
	}
}

func TestDeliveryQueueFlushWithNothingQueuedReturnsAtOnce(t *testing.T) {
	config := &Configuration{DSN: "http://key@127.0.0.1:1/events", QueueSize: 10, Timeout: time.Second, Logger: noopLogger{}}
	queue := NewDeliveryQueue(config, NewClient(config))

	started := time.Now()
	if !queue.Flush(time.Second) {
		t.Fatal("Flush() on an empty queue = false, want true")
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Errorf("Flush() on an empty queue took %v", elapsed)
	}
}

func TestDeliveryQueueFlushGivesUpAtTheTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	defer close(release)

	config := &Configuration{DSN: "http://key@" + server.Listener.Addr().String() + "/events", QueueSize: 10, Timeout: 5 * time.Second, Logger: noopLogger{}}
	queue := NewDeliveryQueue(config, NewClient(config))
	queue.Push(map[string]any{"n": 1})

	started := time.Now()
	if queue.Flush(100 * time.Millisecond) {
		t.Fatal("Flush() = true, want false: the only delivery was still blocked on the server")
	}
	if elapsed := time.Since(started); elapsed < 100*time.Millisecond || elapsed > time.Second {
		t.Errorf("Flush(100ms) returned after %v", elapsed)
	}
}

func TestDeliveryQueueFlushGivesUpWhenTheQueueStaysFull(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	defer close(release)

	config := &Configuration{DSN: "http://key@" + server.Listener.Addr().String() + "/events", QueueSize: 1, Timeout: 5 * time.Second, Logger: noopLogger{}}
	queue := NewDeliveryQueue(config, NewClient(config))
	queue.Push(map[string]any{"n": 1})
	time.Sleep(50 * time.Millisecond)  // the worker is now blocked inside the handler
	queue.Push(map[string]any{"n": 2}) // and this fills the size-1 queue, leaving no room for the marker

	if queue.Flush(100 * time.Millisecond) {
		t.Fatal("Flush() = true, want false: there was never room to queue behind the backlog")
	}
}

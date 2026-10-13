package forgeops

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientDeliverSendsAuthorizedRequest(t *testing.T) {
	var gotAuth, gotContentType, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		body := map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotBody, _ = body["message"].(string)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	config := &Configuration{DSN: "http://secret-key@" + server.Listener.Addr().String() + "/api/v1/events", Timeout: time.Second, Logger: noopLogger{}}
	client := NewClient(config)

	ok := client.Deliver(map[string]any{"message": "boom"})

	if !ok {
		t.Fatal("Deliver() = false, want true")
	}
	if gotAuth != "Bearer secret-key" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer secret-key")
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type header = %q, want application/json", gotContentType)
	}
	if gotBody != "boom" {
		t.Errorf("delivered body message = %q, want %q", gotBody, "boom")
	}
}

func TestClientDeliverReturnsFalseOnNon2xx(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	config := &Configuration{DSN: "http://key@" + server.Listener.Addr().String() + "/events", Timeout: time.Second, Logger: noopLogger{}}
	client := NewClient(config)

	if client.Deliver(map[string]any{"message": "boom"}) {
		t.Error("Deliver() = true, want false for a 500 response")
	}
}

func TestClientDeliverReturnsFalseWhenUnreachable(t *testing.T) {
	config := &Configuration{DSN: "http://key@127.0.0.1:1/events", Timeout: 200 * time.Millisecond, Logger: noopLogger{}}
	client := NewClient(config)

	if client.Deliver(map[string]any{"message": "boom"}) {
		t.Error("Deliver() = true, want false for an unreachable host")
	}
}

func TestClientDeliverReturnsFalseWithNoDSN(t *testing.T) {
	config := &Configuration{Logger: noopLogger{}}
	client := NewClient(config)

	if client.Deliver(map[string]any{"message": "boom"}) {
		t.Error("Deliver() = true, want false with no DSN configured")
	}
}

func TestClientDeliverPerformanceSamplesPostsTheBatchWrappedInSamples(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	config := &Configuration{DSN: "http://key@" + server.Listener.Addr().String() + "/api/v1/events", Timeout: time.Second, Logger: noopLogger{}}
	client := NewClient(config)

	ok := client.DeliverPerformanceSamples([]map[string]any{{"transaction_name": "GET /posts/:id"}})

	if !ok {
		t.Fatal("DeliverPerformanceSamples() = false, want true")
	}
	if gotPath != "/api/v1/performance_samples" {
		t.Errorf("request path = %q, want /api/v1/performance_samples", gotPath)
	}
	samples, _ := gotBody["samples"].([]any)
	if len(samples) != 1 {
		t.Fatalf("samples length = %d, want 1", len(samples))
	}
}

func TestOutcomeForStatusTellsDeliveredRejectedAndFailedForNowApart(t *testing.T) {
	cases := map[int]deliveryOutcome{
		200: deliveryDelivered, 202: deliveryDelivered, 204: deliveryDelivered,
		400: deliveryRejected, 401: deliveryRejected, 403: deliveryRejected, 404: deliveryRejected, 413: deliveryRejected, 422: deliveryRejected,
		408: deliveryFailed, 429: deliveryFailed,
		500: deliveryFailed, 502: deliveryFailed, 503: deliveryFailed,
		302: deliveryFailed,
	}
	for status, want := range cases {
		if got := outcomeForStatus(status); got != want {
			t.Errorf("outcomeForStatus(%d) = %v, want %v", status, got, want)
		}
	}
}

func TestClientPostReportsARejectionAndAFailureForNowDifferently(t *testing.T) {
	var status atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(status.Load()))
	}))
	defer server.Close()

	config := &Configuration{DSN: "http://key@" + server.Listener.Addr().String() + "/api/v1/events", Timeout: time.Second, Logger: noopLogger{}}
	client := NewClient(config)

	for code, want := range map[int32]deliveryOutcome{202: deliveryDelivered, 403: deliveryRejected, 429: deliveryFailed, 503: deliveryFailed} {
		status.Store(code)
		if got := client.deliverMetrics([]map[string]any{{"metric_name": "signup"}}); got != want {
			t.Errorf("deliverMetrics() against a %d = %v, want %v", code, got, want)
		}
	}

	unreachable := NewClient(&Configuration{DSN: "http://key@127.0.0.1:1/api/v1/events", Timeout: 200 * time.Millisecond, Logger: noopLogger{}})
	if got := unreachable.deliverMetrics(nil); got != deliveryFailed {
		t.Errorf("deliverMetrics() against an unreachable host = %v, want deliveryFailed", got)
	}
}

func TestClientPostTreatsAPayloadThatCannotBeEncodedAsRejected(t *testing.T) {
	config := &Configuration{DSN: "http://key@127.0.0.1:1/api/v1/events", Timeout: 200 * time.Millisecond, Logger: noopLogger{}}
	client := NewClient(config)

	if got := client.deliverMetrics([]map[string]any{{"value": make(chan int)}}); got != deliveryRejected {
		t.Errorf("deliverMetrics() = %v, want deliveryRejected: it can never be encoded, so never retried", got)
	}
}

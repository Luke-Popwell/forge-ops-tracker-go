package forgeops

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

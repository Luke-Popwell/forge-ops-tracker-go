package forgeops

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"
)

// strictPrivacyVectors is testdata/strict_privacy_vectors.json, a byte-identical copy of this
// repo's sdks/strict_privacy_vectors.json, generated from the server's own implementation.
type strictPrivacyVectors struct {
	Fingerprint []struct {
		Message     string `json:"message"`
		Normalized  string `json:"normalized"`
		Fingerprint string `json:"fingerprint"`
	} `json:"fingerprint"`
	SpanName []struct {
		Name   string `json:"name"`
		Strict string `json:"strict"`
	} `json:"span_name"`
	Path []struct {
		Path   string `json:"path"`
		Strict string `json:"strict"`
	} `json:"path"`
}

func loadStrictPrivacyVectors(t *testing.T) strictPrivacyVectors {
	t.Helper()
	data, err := os.ReadFile("testdata/strict_privacy_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors strictPrivacyVectors
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors.Fingerprint) == 0 || len(vectors.SpanName) == 0 || len(vectors.Path) == 0 {
		t.Fatal("vectors file is missing a section")
	}
	return vectors
}

func strictConfiguration() *Configuration {
	config := testConfiguration()
	config.StrictPrivacy = true
	config.PrivacyAllowedFields = []string{"error_code", "feature", "nested"}
	config.CaptureSQLObjects = true
	config.CaptureSQLStatement = true
	return config
}

func TestStrictPrivacyFingerprintVectors(t *testing.T) {
	for _, vector := range loadStrictPrivacyVectors(t).Fingerprint {
		if got := normalizeMessage(vector.Message); got != vector.Normalized {
			t.Errorf("normalizeMessage(%q) = %q, want %q", vector.Message, got, vector.Normalized)
		}
		if got := messageFingerprint(vector.Message); got != vector.Fingerprint {
			t.Errorf("messageFingerprint(%q) = %q, want %q", vector.Message, got, vector.Fingerprint)
		}
	}
}

func TestStrictPrivacySpanNameVectors(t *testing.T) {
	for _, vector := range loadStrictPrivacyVectors(t).SpanName {
		if got := strictSpanName(vector.Name); got != vector.Strict {
			t.Errorf("strictSpanName(%q) = %q, want %q", vector.Name, got, vector.Strict)
		}
	}
}

func TestStrictPrivacyPathVectors(t *testing.T) {
	for _, vector := range loadStrictPrivacyVectors(t).Path {
		if got := pathWithoutIDs(vector.Path); got != vector.Strict {
			t.Errorf("pathWithoutIDs(%q) = %q, want %q", vector.Path, got, vector.Strict)
		}
	}
}

func TestStrictPrivacyReadsTheEnvironment(t *testing.T) {
	env := map[string]string{"FORGE_OPS_STRICT_PRIVACY": "true", "FORGE_OPS_PRIVACY_ALLOWED_FIELDS": "error_code, feature,,"}
	on, fields := strictPrivacyFromEnvironment(func(key string) string { return env[key] })
	if !on || !reflect.DeepEqual(fields, []string{"error_code", "feature"}) {
		t.Errorf("got %v %v, want true [error_code feature]", on, fields)
	}

	on, fields = strictPrivacyFromEnvironment(func(string) string { return "" })
	if on || fields != nil {
		t.Errorf("got %v %v with nothing set, want off and no fields", on, fields)
	}

	t.Setenv("FORGE_OPS_STRICT_PRIVACY", "true")
	t.Setenv("FORGE_OPS_PRIVACY_ALLOWED_FIELDS", "error_code")
	config := NewConfiguration()
	if !config.StrictPrivacy || !reflect.DeepEqual(config.PrivacyAllowedFields, []string{"error_code"}) {
		t.Errorf("NewConfiguration() = %v %v, want strict privacy on with error_code", config.StrictPrivacy, config.PrivacyAllowedFields)
	}
}

func TestStrictPrivacyEventSendsNoFreeText(t *testing.T) {
	builder := NewEventBuilder(strictConfiguration())
	err := WithSQL(errors.New("Couldn't find Invoice with id=8821 for jane@example.com"), "SELECT * FROM invoices WHERE id = 8821")
	context := map[string]any{"error_code": "E42", "feature": 7, "nested": map[string]any{"a": 1}, "patient": "Jane Doe"}
	user := map[string]any{"id": 42, "email": "jane@example.com", "username": "jane"}
	crumbs := []Breadcrumb{{Category: "custom", Message: "opened Jane's chart", Level: "info", Data: map[string]any{}}}

	payload := builder.Build(err, context, user, capturePcs(), crumbs)

	if payload["message"] != "" {
		t.Errorf("message = %v, want empty", payload["message"])
	}
	if payload["message_fingerprint"] != messageFingerprint(ScrubString(err.Error())) || len(payload["message_fingerprint"].(string)) != 32 {
		t.Errorf("message_fingerprint = %v", payload["message_fingerprint"])
	}
	for _, key := range []string{"breadcrumbs", "endpoint", "sql_statement", "tags"} {
		if _, present := payload[key]; present {
			t.Errorf("%s = %v, want it left out", key, payload[key])
		}
	}
	if !reflect.DeepEqual(payload["context"], map[string]any{"error_code": "E42", "feature": 7}) {
		t.Errorf("context = %v, want only the allowed plain values", payload["context"])
	}
	if !reflect.DeepEqual(payload["user"], map[string]any{"id": "42"}) {
		t.Errorf("user = %v, want only the id as a string", payload["user"])
	}
	if payload["sql_objects"] == nil {
		t.Errorf("sql_objects = %v, want them kept", payload["sql_objects"])
	}
	if payload["exception_class"] == "" || len(payload["backtrace"].([]Frame)) == 0 {
		t.Errorf("exception_class and backtrace should be kept: %v", payload)
	}
	for _, frame := range payload["backtrace"].([]Frame) {
		if frame.ContextLine != nil || frame.PreContext != nil || frame.PostContext != nil {
			t.Errorf("frame %v carries source context", frame)
		}
	}
}

func TestStrictPrivacyLeavesOutContextUserAndSourceContextWhenNothingIsLeft(t *testing.T) {
	config := strictConfiguration()
	config.PrivacyAllowedFields = nil
	builder := NewEventBuilder(config)

	payload := builder.Build(errors.New("boom"), map[string]any{"error_code": "E42"}, map[string]any{"email": "jane@example.com"}, capturePcs(), nil)
	for _, key := range []string{"context", "tags", "user"} {
		if _, present := payload[key]; present {
			t.Errorf("%s = %v, want it left out", key, payload[key])
		}
	}
	if payload["message_fingerprint"] != "81f52337ebb4cb1669bb802c708807dd" {
		t.Errorf("message_fingerprint = %v", payload["message_fingerprint"])
	}

	original := readFile
	readFile = func(string) ([]byte, error) {
		t.Fatal("readFile should not be called under strict privacy")
		return nil, nil
	}
	defer func() { readFile = original }()
	if frame := builder.attachSourceContext(Frame{File: "/app/main.go", Line: 1, InApp: true}); frame.ContextLine != nil {
		t.Errorf("frame = %v, want no source context", frame)
	}

	context, pre := "line", []string{"before"}
	stripped := applyStrictPrivacy(map[string]any{"message": "boom", "backtrace": []Frame{{File: "/app/main.go", Line: 3, Method: "main.run", InApp: true, ContextLine: &context, PreContext: &pre, PostContext: &pre}}}, nil)
	if !reflect.DeepEqual(stripped["backtrace"], []Frame{{File: "/app/main.go", Line: 3, Method: "main.run", InApp: true}}) {
		t.Errorf("backtrace = %v, want file, line, method and in_app only", stripped["backtrace"])
	}
}

func TestStrictPrivacyOffLeavesEventsAsBefore(t *testing.T) {
	config := testConfiguration()
	config.CaptureSQLObjects = true
	config.CaptureSQLStatement = true
	builder := NewEventBuilder(config)
	err := WithSQL(errors.New("Couldn't find Invoice with id=8821"), "SELECT * FROM invoices WHERE id = 8821")
	crumbs := []Breadcrumb{{Category: "custom", Message: "opened the chart", Level: "info", Data: map[string]any{}}}

	payload := builder.Build(err, map[string]any{"patient": "Jane Doe"}, map[string]any{"id": 42, "email": "jane@example.com"}, capturePcs(), crumbs)

	if payload["message"] != "Couldn't find Invoice with id=8821" {
		t.Errorf("message = %v", payload["message"])
	}
	if _, present := payload["message_fingerprint"]; present {
		t.Error("message_fingerprint should only be sent under strict privacy")
	}
	if payload["sql_statement"] != "SELECT * FROM invoices WHERE id = ?" {
		t.Errorf("sql_statement = %v", payload["sql_statement"])
	}
	if !reflect.DeepEqual(payload["context"], map[string]any{"patient": "Jane Doe"}) || !reflect.DeepEqual(payload["tags"], map[string]any{}) {
		t.Errorf("context = %v, tags = %v", payload["context"], payload["tags"])
	}
	if !reflect.DeepEqual(payload["user"], map[string]any{"id": 42, "email": "jane@example.com"}) {
		t.Errorf("user = %v", payload["user"])
	}
	if len(payload["breadcrumbs"].([]Breadcrumb)) != 1 {
		t.Errorf("breadcrumbs = %v", payload["breadcrumbs"])
	}
}

// eventsServer records every event body delivered to it.
type eventsServer struct {
	*httptest.Server
	mu     sync.Mutex
	events []map[string]any
}

func newEventsServer(t *testing.T) *eventsServer {
	t.Helper()
	s := &eventsServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.events = append(s.events, body)
		s.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *eventsServer) waitForEvent(t *testing.T) map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		if len(s.events) > 0 {
			event := s.events[0]
			s.mu.Unlock()
			return event
		}
		s.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for an event")
	return nil
}

func TestStrictPrivacyReportSendsNoEndpointButKeepsTheTransactionName(t *testing.T) {
	for _, strict := range []bool{true, false} {
		server := newEventsServer(t)
		config := &Configuration{
			DSN:                 "http://key@" + server.Listener.Addr().String() + "/events",
			Environment:         "production",
			EnabledEnvironments: map[string]bool{"production": true},
			Timeout:             time.Second,
			Logger:              noopLogger{},
			StrictPrivacy:       strict,
		}
		reporter := NewReporter(config, NewEventBuilder(config), NewDeliveryQueue(config, NewClient(config)))

		reporter.report(errors.New("boom"), nil, nil, capturePcs(), nil, traceFields{transactionName: "GET /patients/{id}", endpoint: "GET /patients/{id}", traceID: "abc"})

		event := server.waitForEvent(t)
		if event["transaction_name"] != "GET /patients/{id}" || event["trace_id"] != "abc" {
			t.Errorf("strict=%v: event = %v, want transaction_name and trace_id kept", strict, event)
		}
		if _, present := event["endpoint"]; present == strict {
			t.Errorf("strict=%v: endpoint present = %v", strict, present)
		}
	}
}

func TestStrictPrivacyRecordsNoBreadcrumbs(t *testing.T) {
	server := newSpansServer(t)
	initTracing(t, server, func(c *Configuration) { c.StrictPrivacy = true })

	ctx := WithBreadcrumbs(context.Background())
	AddBreadcrumb(ctx, "opened Jane's chart", "", "", nil)
	if crumbs := breadcrumbsFromContext(ctx); crumbs != nil {
		t.Errorf("breadcrumbs = %v, want none recorded", crumbs)
	}
}

func TestStrictPrivacySpansKeepOnlyMethodHostAndStatementData(t *testing.T) {
	server := newSpansServer(t)
	initTracing(t, server, func(c *Configuration) { c.StrictPrivacy = true })

	start := time.Now()
	ctx := WithTrace(context.Background())
	RecordSpan(ctx, "GET https://api.example.com/patients/42?x=1", "http", time.Now(), time.Millisecond, map[string]any{"status": 200, "url": "https://api.example.com/patients/42"})
	RecordDatabaseSpan(ctx, "SELECT patients", "SELECT * FROM patients WHERE name = 'Jane'", "postgresql", time.Now(), time.Millisecond)
	_, end := StartSpan(ctx, "charge card", "service", map[string]any{"patient": "Jane Doe"})
	end()
	FinishTrace(ctx, "GET /patients/42/notes", start, 1500*time.Millisecond)

	trace := server.waitForTraces(t, 1)[0]
	if _, present := spanNamed(t, trace, "GET api.example.com")["data"]; present {
		t.Error("the http span should carry no data")
	}
	if _, present := spanNamed(t, trace, "charge card")["data"]; present {
		t.Error("the service span should carry no data")
	}
	query := spanNamed(t, trace, "SELECT patients")
	if !reflect.DeepEqual(query["data"], map[string]any{"db.statement": "SELECT * FROM patients WHERE name = ?", "db.system": "postgresql"}) {
		t.Errorf("database span data = %v", query["data"])
	}
	if root := spanNamed(t, trace, "GET /patients/:id/notes"); root["kind"] != "controller" {
		t.Errorf("root = %v", root)
	}
}

func TestStrictPrivacyOffLeavesSpansAsBefore(t *testing.T) {
	server := newSpansServer(t)
	initTracing(t, server, nil)

	start := time.Now()
	ctx := WithTrace(context.Background())
	RecordSpan(ctx, "GET api.example.com", "http", time.Now(), time.Millisecond, map[string]any{"status": 200})
	FinishTrace(ctx, "GET /patients/42/notes", start, 1500*time.Millisecond)

	trace := server.waitForTraces(t, 1)[0]
	if !reflect.DeepEqual(spanNamed(t, trace, "GET api.example.com")["data"], map[string]any{"status": float64(200)}) {
		t.Error("the http span should keep its data")
	}
	spanNamed(t, trace, "GET /patients/42/notes")
}

func TestStrictPrivacyTransactionNamesLoseRawPathIDs(t *testing.T) {
	cases := map[string]string{
		"GET /patients/42/notes":   "GET /patients/:id/notes",
		"GET /patients/{id}":       "GET /patients/{id}",
		"OrdersController#create":  "OrdersController#create",
		"/patients/jane-doe-1961":  "/patients/:id",
		"":                         "",
		"invoicing.SyncWorker.Run": "invoicing.SyncWorker.Run",
	}
	for name, want := range cases {
		if got := strictTransactionName(name); got != want {
			t.Errorf("strictTransactionName(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestStrictPrivacyReportReducesARawPathTransactionName(t *testing.T) {
	for _, strict := range []bool{true, false} {
		server := newEventsServer(t)
		config := &Configuration{
			DSN:                 "http://key@" + server.Listener.Addr().String() + "/events",
			Environment:         "production",
			EnabledEnvironments: map[string]bool{"production": true},
			Timeout:             time.Second,
			Logger:              noopLogger{},
			StrictPrivacy:       strict,
		}
		reporter := NewReporter(config, NewEventBuilder(config), NewDeliveryQueue(config, NewClient(config)))

		reporter.report(errors.New("boom"), nil, nil, capturePcs(), nil, traceFields{transactionName: "GET /patients/42/notes"})

		want := "GET /patients/42/notes"
		if strict {
			want = "GET /patients/:id/notes"
		}
		if got := server.waitForEvent(t)["transaction_name"]; got != want {
			t.Errorf("strict=%v: transaction_name = %v, want %q", strict, got, want)
		}
	}
}

func TestStrictPrivacyPerformanceSamplesReduceARawPathTransactionName(t *testing.T) {
	for _, strict := range []bool{true, false} {
		config := testConfiguration()
		config.StrictPrivacy = strict
		flusher := NewPerformanceFlusher(config, NewClient(config))

		flusher.Record("GET /patients/42/notes", 12)
		flusher.Record("GET /orders/{id}", 12)

		flusher.mu.Lock()
		_, raw := flusher.buckets["GET /patients/42/notes"]
		_, reduced := flusher.buckets["GET /patients/:id/notes"]
		_, route := flusher.buckets["GET /orders/{id}"]
		flusher.mu.Unlock()
		if raw == strict || reduced != strict || !route {
			t.Errorf("strict=%v: raw %v, reduced %v, route %v", strict, raw, reduced, route)
		}
	}
}

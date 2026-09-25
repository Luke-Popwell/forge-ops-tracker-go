package forgeops

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type capturedRequest struct {
	path string
	auth string
	body map[string]any
}

// changeServer records every request it receives and answers with status, so a test can check
// both what was sent and that a rejection (a 403 on a plan without change tracking, a 500) never
// surfaces to the caller.
func changeServer(t *testing.T, status int) (*httptest.Server, func() []capturedRequest) {
	t.Helper()
	var mu sync.Mutex
	var requests []capturedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		requests = append(requests, capturedRequest{path: r.URL.Path, auth: r.Header.Get("Authorization"), body: body})
		mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(server.Close)
	return server, func() []capturedRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]capturedRequest(nil), requests...)
	}
}

func initChangeTracking(t *testing.T, server *httptest.Server, configure func(*Configuration)) {
	t.Helper()
	resetForTesting()
	t.Cleanup(resetForTesting)
	Init(func(c *Configuration) {
		c.DSN = "http://secret-key@" + server.Listener.Addr().String() + "/api/v1/events"
		c.Environment = "production"
		c.Timeout = time.Second
		c.Logger = noopLogger{}
		if configure != nil {
			configure(c)
		}
	})
}

func waitForRequests(requests func() []capturedRequest, path string, want int) []capturedRequest {
	deadline := time.Now().Add(2 * time.Second)
	for {
		var matching []capturedRequest
		for _, request := range requests() {
			if request.path == path {
				matching = append(matching, request)
			}
		}
		if len(matching) >= want || time.Now().After(deadline) {
			return matching
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRecordChangeSendsTheFullPayloadToChanges(t *testing.T) {
	server, requests := changeServer(t, http.StatusAccepted)
	initChangeTracking(t, server, func(c *Configuration) { c.DetectChanges = false })

	occurredAt := time.Date(2026, 9, 25, 14, 30, 0, 0, time.FixedZone("EDT", -4*60*60))
	RecordChange(ChangeKindFeatureFlag, "Enabled new checkout", &ChangeOptions{
		Details:    map[string]any{"flag": "new_checkout", "enabled": true},
		Service:    "billing",
		Actor:      "luke",
		URL:        "https://example.com/pull/1",
		ID:         "flag-flip-1",
		OccurredAt: occurredAt,
	})

	got := waitForRequests(requests, "/api/v1/changes", 1)
	if len(got) != 1 {
		t.Fatalf("received %d change requests, want 1", len(got))
	}
	if got[0].auth != "Bearer secret-key" {
		t.Errorf("Authorization = %q, want the DSN's key as a bearer token", got[0].auth)
	}
	body := got[0].body
	want := map[string]any{
		"kind":        "feature_flag",
		"title":       "Enabled new checkout",
		"environment": "production",
		"service":     "billing",
		"actor":       "luke",
		"url":         "https://example.com/pull/1",
		"id":          "flag-flip-1",
		"occurred_at": "2026-09-25T18:30:00Z",
	}
	for key, value := range want {
		if body[key] != value {
			t.Errorf("%s = %v, want %v", key, body[key], value)
		}
	}
	details, _ := body["details"].(map[string]any)
	if details["flag"] != "new_checkout" || details["enabled"] != true {
		t.Errorf("details = %v, want the given object", body["details"])
	}
}

func TestRecordChangeLeavesOutUnsetOptionalFieldsAndDefaultsOccurredAt(t *testing.T) {
	config := &Configuration{Environment: "staging"}

	payload := buildChangePayload(config, ChangeKindConfig, "Raised the pool size", nil)

	if payload["environment"] != "staging" {
		t.Errorf("environment = %v, want the configured environment", payload["environment"])
	}
	for _, key := range []string{"service", "actor", "url", "id", "details"} {
		if _, ok := payload[key]; ok {
			t.Errorf("payload has %q, want it left out when unset", key)
		}
	}
	occurredAt, err := time.Parse(time.RFC3339, payload["occurred_at"].(string))
	if err != nil || time.Since(occurredAt) > time.Minute {
		t.Errorf("occurred_at = %v, want roughly now as RFC 3339", payload["occurred_at"])
	}
}

func TestRecordChangeValidatesKindAndTitle(t *testing.T) {
	config := &Configuration{Environment: "production"}

	for _, kind := range []string{"feature_flag", "config", "migration", "dependency", "infrastructure", "other"} {
		if got := buildChangePayload(config, kind, "t", nil)["kind"]; got != kind {
			t.Errorf("kind %q sent as %v, want it unchanged", kind, got)
		}
	}
	for _, kind := range []string{"", "deploy", "FEATURE_FLAG"} {
		if got := buildChangePayload(config, kind, "t", nil)["kind"]; got != "other" {
			t.Errorf("kind %q sent as %v, want other", kind, got)
		}
	}
	if payload := buildChangePayload(config, "config", "   ", nil); payload != nil {
		t.Errorf("blank title built %v, want nil (dropped)", payload)
	}
	long := strings.Repeat("é", 250)
	if got := buildChangePayload(config, "config", long, nil)["title"].(string); len([]rune(got)) != 200 {
		t.Errorf("title length = %d characters, want 200", len([]rune(got)))
	}
}

func TestRecordChangeIsANoOpWhenNotEnabled(t *testing.T) {
	server, requests := changeServer(t, http.StatusAccepted)
	initChangeTracking(t, server, func(c *Configuration) { c.Environment = "development" })

	RecordChange(ChangeKindConfig, "Ignored", nil)
	time.Sleep(50 * time.Millisecond)

	if got := requests(); len(got) != 0 {
		t.Errorf("received %d requests, want none when the environment isn't enabled", len(got))
	}
}

func TestRecordChangeNeverPanicsOnRejectionOrUnreachableServer(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusInternalServerError} {
		server, requests := changeServer(t, status)
		initChangeTracking(t, server, func(c *Configuration) { c.DetectChanges = false })

		RecordChange(ChangeKindConfig, "Rejected", nil)

		if got := waitForRequests(requests, "/api/v1/changes", 1); len(got) != 1 {
			t.Fatalf("status %d: received %d change requests, want 1", status, len(got))
		}
	}

	resetForTesting()
	t.Cleanup(resetForTesting)
	Init(func(c *Configuration) {
		c.DSN = "http://key@127.0.0.1:1/api/v1/events"
		c.Environment = "production"
		c.Timeout = 100 * time.Millisecond
		c.Logger = noopLogger{}
	})
	RecordChange(ChangeKindConfig, "Unreachable", &ChangeOptions{Details: map[string]any{"bad": func() {}}})
	time.Sleep(150 * time.Millisecond)
}

func TestInitSendsOneChangeSnapshotPerProcess(t *testing.T) {
	server, requests := changeServer(t, http.StatusAccepted)
	initChangeTracking(t, server, nil)
	Init(nil)
	Init(func(c *Configuration) { c.Release = "v2" })

	waitForRequests(requests, "/api/v1/change_snapshots", 1)
	time.Sleep(50 * time.Millisecond) // room for a second snapshot to arrive, if one were sent
	got := waitForRequests(requests, "/api/v1/change_snapshots", 1)
	if len(got) != 1 {
		t.Fatalf("received %d snapshots, want exactly 1 across repeated Init calls", len(got))
	}
	body := got[0].body
	if body["environment"] != "production" {
		t.Errorf("environment = %v, want production", body["environment"])
	}
	if _, ok := body["service"]; ok {
		t.Error("service sent, want it left out (this client has no service setting)")
	}
	state, _ := body["state"].(map[string]any)
	if runtime, _ := state["runtime"].(string); !strings.HasPrefix(runtime, "go 1.") {
		t.Errorf("state.runtime = %q, want \"go <version>\"", runtime)
	}
	if _, ok := state["dependencies"].(map[string]any); !ok {
		t.Errorf("state.dependencies = %v, want a name to version object", state["dependencies"])
	}
	if _, ok := state["env_var_names"]; ok {
		t.Error("env_var_names sent without TrackEnvVarNames")
	}
	if _, ok := state["schema_version"]; ok {
		t.Error("schema_version sent, want it left out")
	}
}

func TestInitSendsNoSnapshotWithDetectChangesOff(t *testing.T) {
	server, requests := changeServer(t, http.StatusAccepted)
	initChangeTracking(t, server, func(c *Configuration) { c.DetectChanges = false })

	time.Sleep(100 * time.Millisecond)

	if got := requests(); len(got) != 0 {
		t.Errorf("received %d requests, want none with DetectChanges off", len(got))
	}
}

func TestInitSendsNoSnapshotWhenNotEnabled(t *testing.T) {
	server, requests := changeServer(t, http.StatusAccepted)
	initChangeTracking(t, server, func(c *Configuration) { c.Environment = "development" })

	time.Sleep(100 * time.Millisecond)

	if got := requests(); len(got) != 0 {
		t.Errorf("received %d requests, want none when the environment isn't enabled", len(got))
	}
}

func TestChangeSnapshotSendsEnvVarNamesOnlyWhenOptedIn(t *testing.T) {
	t.Setenv("STRIPE_SECRET_KEY", "sk_live_do_not_send")
	t.Setenv("FORGE_OPS_RELEASE", "abc")

	if _, ok := buildChangeSnapshot(&Configuration{Environment: "production"})["state"].(map[string]any)["env_var_names"]; ok {
		t.Error("env_var_names present with TrackEnvVarNames off")
	}

	snapshot := buildChangeSnapshot(&Configuration{Environment: "production", TrackEnvVarNames: true})
	names, _ := snapshot["state"].(map[string]any)["env_var_names"].([]string)
	if !containsString(names, "STRIPE_SECRET_KEY") {
		t.Errorf("env_var_names = %v, want it to include STRIPE_SECRET_KEY", names)
	}
	if containsString(names, "FORGE_OPS_RELEASE") {
		t.Error("env_var_names includes this client's own FORGE_OPS_ variable")
	}
	encoded, _ := json.Marshal(snapshot)
	if strings.Contains(string(encoded), "sk_live_do_not_send") {
		t.Error("snapshot contains an environment variable's value")
	}
}

func TestTrackedEnvVarNamesAppliesTheDenylist(t *testing.T) {
	environ := []string{
		"DATABASE_URL=postgres://x", "HOSTNAME=web-1", "HOST=web-1", "HOME=/root", "PATH=/bin",
		"PWD=/app", "OLDPWD=/", "SHLVL=1", "_=/usr/bin/app", "TERM=xterm", "USER=app", "LOGNAME=app",
		"SHELL=/bin/sh", "LANG=C", "LC_ALL=C", "TMPDIR=/tmp", "TZ=UTC", "PORT=3000", "DYNO=web.1",
		"INVOCATION_ID=abc", "JOURNAL_STREAM=8:1", "SYSTEMD_EXEC_PID=1", "MEMORY_PRESSURE_WATCH=/x",
		"KUBERNETES_SERVICE_HOST=10.0.0.1", "REDIS_SERVICE_HOST=10.0.0.2",
		"REDIS_SERVICE_PORT_HTTP=6379", "REDIS_PORT_6379_TCP_ADDR=10.0.0.2", "FORGE_OPS_DSN=secret",
		"=C:=C:\\", "FEATURE_X=1", "DATABASE_URL=duplicate",
	}

	got := trackedEnvVarNames(environ)

	if strings.Join(got, ",") != "DATABASE_URL,FEATURE_X" {
		t.Errorf("trackedEnvVarNames = %v, want [DATABASE_URL FEATURE_X]", got)
	}
}

func TestBuildDependenciesCapsNamesAndVersions(t *testing.T) {
	dependencies, ok := buildDependencies()
	if !ok {
		t.Skip("no build info in this test binary")
	}
	if len(dependencies) > maxSnapshotEntries {
		t.Errorf("%d dependencies, want at most %d", len(dependencies), maxSnapshotEntries)
	}
	for name, version := range dependencies {
		if name == "" || version == "" {
			t.Errorf("dependency %q = %q, want neither empty", name, version)
		}
	}
	if truncateRunes(strings.Repeat("a", 500), maxDependencyNameLength) != strings.Repeat("a", 200) {
		t.Error("truncateRunes did not cut to the limit")
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

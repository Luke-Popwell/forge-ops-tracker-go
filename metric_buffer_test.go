package forgeops

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func newMetricConfiguration(dsn string) *Configuration {
	return &Configuration{
		DSN: dsn, Release: "1.2.3", Environment: "production", ServerName: "web-1", Timeout: time.Second, Logger: noopLogger{},
		EnabledEnvironments:               map[string]bool{"production": true},
		MetricFlushInterval:               time.Hour,
		InfrastructureMetricFlushInterval: time.Hour,
	}
}

func newTestBuffer(deliver func([]map[string]any) bool) *MetricBuffer {
	return NewMetricBuffer(newMetricConfiguration("https://key@tracker.example.com/api/v1/events"), deliver, func() time.Duration { return time.Hour })
}

func names(entries []map[string]any) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e["metric_name"].(string)
	}
	return out
}

func TestMetricBufferDeliversEveryEntryAsOneBatchStampedWithRecordedAt(t *testing.T) {
	var delivered [][]map[string]any
	buffer := newTestBuffer(func(entries []map[string]any) bool { delivered = append(delivered, entries); return true })

	buffer.Record(map[string]any{"metric_name": "signup", "value": 1.0})
	buffer.Record(map[string]any{"metric_name": "payment", "value": 49.5})
	buffer.Flush()

	if len(delivered) != 1 || len(delivered[0]) != 2 {
		t.Fatalf("delivered = %v", delivered)
	}
	if got := delivered[0][0]["recorded_at"].(string); len(got) != 20 || got[19] != 'Z' {
		t.Errorf("recorded_at = %q", got)
	}
}

func TestMetricBufferKeepsANegativeValueSinceARefundIsARealMetric(t *testing.T) {
	var delivered []map[string]any
	buffer := newTestBuffer(func(entries []map[string]any) bool { delivered = entries; return true })
	buffer.Record(map[string]any{"metric_name": "refund", "value": -12.0})
	buffer.Flush()
	if delivered[0]["value"] != -12.0 {
		t.Errorf("value = %v", delivered[0]["value"])
	}
}

func TestMetricBufferDropsNaNInfiniteAndNonNumericValuesSoOneBadEntryCannotPoisonABatch(t *testing.T) {
	buffer := newTestBuffer(func([]map[string]any) bool { return true })
	for name, value := range map[string]any{"nan": math.NaN(), "inf": math.Inf(1), "string": "12", "int": 3} {
		if buffer.Record(map[string]any{"metric_name": name, "value": value}) {
			t.Errorf("%s should have been dropped", name)
		}
	}
	if !buffer.Record(map[string]any{"metric_name": "ok", "value": 3.0}) {
		t.Error("a finite float64 should be kept")
	}
}

func TestMetricBufferAFailedDeliveryKeepsEveryEntryForTheNextFlush(t *testing.T) {
	outcomes := []bool{false, true}
	var delivered [][]string
	buffer := newTestBuffer(func(entries []map[string]any) bool {
		delivered = append(delivered, names(entries))
		ok := outcomes[0]
		outcomes = outcomes[1:]
		return ok
	})

	buffer.Record(map[string]any{"metric_name": "a", "value": 1.0})
	buffer.Flush()
	buffer.Record(map[string]any{"metric_name": "b", "value": 2.0})
	buffer.Flush()
	buffer.Flush() // nothing left: no third delivery

	if len(delivered) != 2 || len(delivered[1]) != 2 || delivered[1][0] != "a" || delivered[1][1] != "b" {
		t.Fatalf("delivered = %v", delivered)
	}
}

func TestMetricBufferAnEntryRecordedWhileDeliveryIsInFlightIsNeverLost(t *testing.T) {
	inFlight := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	var delivered [][]string
	buffer := newTestBuffer(func(entries []map[string]any) bool {
		mu.Lock()
		delivered = append(delivered, names(entries))
		first := len(delivered) == 1
		mu.Unlock()
		if first {
			close(inFlight)
			<-release
		}
		return true
	})

	buffer.Record(map[string]any{"metric_name": "first", "value": 1.0})
	done := make(chan struct{})
	go func() { buffer.Flush(); close(done) }()
	<-inFlight
	buffer.Record(map[string]any{"metric_name": "during", "value": 2.0})
	close(release)
	<-done
	buffer.Flush()

	if len(delivered) != 2 || delivered[0][0] != "first" || len(delivered[1]) != 1 || delivered[1][0] != "during" {
		t.Fatalf("delivered = %v", delivered)
	}
}

func TestMetricBufferIsCappedAndDropsFurtherEntriesUntilAFlushSucceeds(t *testing.T) {
	buffer := newTestBuffer(func([]map[string]any) bool { return false })
	accepted := 0
	for i := 0; i < MaxMetricEntries+50; i++ {
		if buffer.Record(map[string]any{"metric_name": "m", "value": 1.0}) {
			accepted++
		}
	}
	if accepted != MaxMetricEntries {
		t.Errorf("accepted = %d, want %d", accepted, MaxMetricEntries)
	}
}

func TestCaptureMetricAndCaptureInfrastructureMetricDeliverToTheirOwnEndpointsEndToEnd(t *testing.T) {
	var mu sync.Mutex
	bodies := map[string]map[string]any{}
	auth := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies[r.URL.Path] = body
		auth = r.Header.Get("Authorization")
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	resetForTesting()
	defer resetForTesting()
	Init(func(c *Configuration) {
		c.DetectChanges = false
		c.DSN = "http://key@" + server.Listener.Addr().String() + "/api/v1/events"
		c.Environment = "production"
		c.Release = "a1b2c3d"
		c.ServerName = "web-1"
		c.EnabledEnvironments = map[string]bool{"production": true}
	})
	CaptureMetric("signup", 1)
	CaptureMetric("payment", 49)
	CaptureInfrastructureMetric("cpu", 0.42, "db-1")
	CaptureInfrastructureMetric("memory", 0.7, "")
	FlushMetrics()

	mu.Lock()
	defer mu.Unlock()
	custom := bodies["/api/v1/custom_metrics"]["metrics"].([]any)
	if len(custom) != 2 || custom[0].(map[string]any)["metric_name"] != "signup" || custom[1].(map[string]any)["value"] != 49.0 {
		t.Fatalf("custom = %v", custom)
	}
	if custom[0].(map[string]any)["environment"] != "production" || custom[0].(map[string]any)["release"] != "a1b2c3d" {
		t.Errorf("custom[0] = %v", custom[0])
	}
	infra := bodies["/api/v1/infrastructure_metrics"]["metrics"].([]any)
	if infra[0].(map[string]any)["hostname"] != "db-1" || infra[1].(map[string]any)["hostname"] != "web-1" {
		t.Errorf("infra = %v", infra)
	}
	if auth != "Bearer key" {
		t.Errorf("auth = %q", auth)
	}
}

func TestCaptureMetricIsANoOpWhenTheClientIsNotEnabled(t *testing.T) {
	resetForTesting()
	defer resetForTesting()
	Init(func(c *Configuration) {
		c.DetectChanges = false
		c.DSN = "https://key@tracker.example.com/api/v1/events"
		c.Environment = "development"
	})
	CaptureMetric("signup", 1)
	CaptureInfrastructureMetric("cpu", 1, "")
	_, metrics, infra := metricState()
	if len(metrics.entries) != 0 || len(infra.entries) != 0 {
		t.Error("nothing should have been buffered")
	}
}

func TestMetricURIsSwapTheTrailingEventsSegment(t *testing.T) {
	config := newMetricConfiguration("https://key@tracker.example.com/api/v1/events")
	if got := config.CustomMetricsURI(); got != "https://tracker.example.com/api/v1/custom_metrics" {
		t.Errorf("custom = %q", got)
	}
	if got := config.InfrastructureMetricsURI(); got != "https://tracker.example.com/api/v1/infrastructure_metrics" {
		t.Errorf("infrastructure = %q", got)
	}
}

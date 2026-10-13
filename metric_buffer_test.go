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

func newOutcomeBuffer(logger Logger, deliver func([]map[string]any) deliveryOutcome) *MetricBuffer {
	config := newMetricConfiguration("https://key@tracker.example.com/api/v1/events")
	config.Logger = logger
	return newMetricBuffer(config, deliver, func() time.Duration { return time.Hour })
}

func TestMetricBufferDropsABatchTheServerRejectsAndNeverSendsItAgain(t *testing.T) {
	var delivered [][]string
	logger := &recordingLogger{}
	buffer := newOutcomeBuffer(logger, func(entries []map[string]any) deliveryOutcome {
		delivered = append(delivered, names(entries))
		return deliveryRejected
	})

	buffer.Record(map[string]any{"metric_name": "a", "value": 1.0})
	buffer.Flush()
	buffer.Flush() // nothing left: no second delivery
	buffer.Record(map[string]any{"metric_name": "b", "value": 2.0})
	buffer.Flush()

	if len(delivered) != 2 || len(delivered[1]) != 1 || delivered[1][0] != "b" {
		t.Fatalf("delivered = %v, want [[a] [b]]: a rejected entry is never sent again", delivered)
	}
	want := []string{"dropped 1 metrics: the server rejected them, and would again", "dropped 1 metrics: the server rejected them, and would again"}
	if len(logger.lines) != 2 || logger.lines[0] != want[0] || logger.lines[1] != want[1] {
		t.Errorf("log lines = %q, want one per dropped batch", logger.lines)
	}
}

func TestMetricBufferThatFilledUpTakesNewMetricsAgainOnceARejectedBatchIsDropped(t *testing.T) {
	buffer := newOutcomeBuffer(noopLogger{}, func([]map[string]any) deliveryOutcome { return deliveryRejected })
	for i := 0; i < MaxMetricEntries; i++ {
		buffer.Record(map[string]any{"metric_name": "m", "value": 1.0})
	}
	if buffer.Record(map[string]any{"metric_name": "m", "value": 1.0}) {
		t.Fatal("a full buffer should drop a further entry")
	}

	buffer.Flush()

	if !buffer.Record(map[string]any{"metric_name": "m", "value": 1.0}) {
		t.Error("the buffer should take entries again after its rejected batch was dropped")
	}
}

func TestMetricBufferDropsTheBatchOnTheFifthFailedFlushInARow(t *testing.T) {
	var delivered [][]string
	logger := &recordingLogger{}
	buffer := newOutcomeBuffer(logger, func(entries []map[string]any) deliveryOutcome {
		delivered = append(delivered, names(entries))
		return deliveryFailed
	})

	buffer.Record(map[string]any{"metric_name": "a", "value": 1.0})
	for flush := 1; flush <= 4; flush++ {
		buffer.Flush()
		if len(buffer.entries) != 1 {
			t.Fatalf("entries after failed flush %d = %d, want the entry kept", flush, len(buffer.entries))
		}
	}
	if len(logger.lines) != 0 {
		t.Fatalf("log lines before the fifth failure = %q, want none", logger.lines)
	}

	buffer.Flush() // the fifth in a row
	if len(buffer.entries) != 0 {
		t.Fatalf("entries after the fifth failed flush = %d, want none", len(buffer.entries))
	}
	buffer.Flush()
	if len(delivered) != 5 {
		t.Errorf("deliveries = %d, want 5 (nothing left to send after the drop)", len(delivered))
	}
	if len(logger.lines) != 1 || logger.lines[0] != "dropped 1 metrics: delivery failed 5 flushes in a row" {
		t.Errorf("log lines = %q", logger.lines)
	}

	// Starts clean: the next batch gets five flushes of its own.
	buffer.Record(map[string]any{"metric_name": "b", "value": 2.0})
	buffer.Flush()
	if len(buffer.entries) != 1 {
		t.Errorf("entries after the first failed flush of a new batch = %d, want 1", len(buffer.entries))
	}
}

func TestMetricBufferCountsOnlyFailedFlushesInARow(t *testing.T) {
	outcome := deliveryFailed
	var last []string
	buffer := newOutcomeBuffer(noopLogger{}, func(entries []map[string]any) deliveryOutcome {
		last = names(entries)
		return outcome
	})

	for round := 0; round < 2; round++ {
		outcome = deliveryFailed
		for flush := 0; flush < 4; flush++ {
			buffer.Record(map[string]any{"metric_name": "m", "value": 1.0})
			buffer.Flush()
		}
		outcome = deliveryDelivered
		buffer.Flush() // delivers all four, and resets the count of failures

		if len(last) != 4 || len(buffer.entries) != 0 {
			t.Fatalf("round %d delivered %d entries with %d left, want 4 and 0 (nothing dropped)", round, len(last), len(buffer.entries))
		}
	}
}

func TestNewMetricBufferTreatsAFalseFromItsDeliverAsFailedForNow(t *testing.T) {
	buffer := newTestBuffer(func([]map[string]any) bool { return false })
	buffer.Record(map[string]any{"metric_name": "a", "value": 1.0})
	buffer.Flush()
	if len(buffer.entries) != 1 {
		t.Errorf("entries = %d, want the entry kept for the next flush", len(buffer.entries))
	}
}

func TestCaptureMetricIsNotSentAgainAfterThePlanRejectsIt(t *testing.T) {
	var mu sync.Mutex
	requests := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests[r.URL.Path]++
		mu.Unlock()
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	resetForTesting()
	defer resetForTesting()
	Init(func(c *Configuration) {
		c.DetectChanges = false
		c.DSN = "http://key@" + server.Listener.Addr().String() + "/api/v1/events"
		c.Environment = "production"
		c.EnabledEnvironments = map[string]bool{"production": true}
	})
	CaptureMetric("signup", 1)
	CaptureInfrastructureMetric("cpu", 0.42, "db-1")
	FlushMetrics()
	FlushMetrics()

	mu.Lock()
	defer mu.Unlock()
	if requests["/api/v1/custom_metrics"] != 1 || requests["/api/v1/infrastructure_metrics"] != 1 {
		t.Errorf("requests = %v, want one per endpoint: a 403 is never retried", requests)
	}
}

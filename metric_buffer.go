package forgeops

import (
	"math"
	"sync"
	"time"
)

// MaxMetricEntries caps one MetricBuffer; see MetricBuffer.
const MaxMetricEntries = 1000

// MetricBuffer collects individual CaptureMetric/CaptureInfrastructureMetric calls in-process and
// periodically flushes them as one batch, rather than one network call per capture. Unlike
// PerformanceFlusher it keeps a list of individually meaningful entries instead of summing them
// into buckets: a customer's own signup or payment is exactly the kind of thing they will want a
// genuinely accurate count/sum of later, so the server stores one row per entry as-is. Ported from
// gems/forge_ops_tracker's metric_buffer.rb and infrastructure_metric_buffer.rb, which are the same
// class twice; here it is one type instantiated twice, told which delivery function and flush
// interval to use.
//
// Three deliberate differences from the Ruby buffers:
//
//   - A flush snapshots the first N entries and, on success, removes exactly those N, instead of
//     resetting the whole list, so an entry recorded while the request is in flight (the lock is
//     released around the network call) is kept for the next flush rather than lost.
//   - The buffer is capped at MaxMetricEntries, and once full further entries are dropped until a
//     flush succeeds: a plan without the feature answers 403 on every flush, and an uncapped buffer
//     would then grow for as long as the process lives. Dropping the newest rather than the oldest
//     keeps the entries a flush is delivering at the front of the slice, which is what makes
//     removing exactly them afterward exact.
//   - A NaN or infinite value is dropped at record time: encoding/json refuses to marshal one, and
//     one bad entry would make every batch behind it fail to send.
//
// Same sync.Once-guarded lazy goroutine start as PerformanceFlusher, for the same reason.
type MetricBuffer struct {
	configuration *Configuration
	deliver       func(entries []map[string]any) bool
	interval      func() time.Duration
	mu            sync.Mutex
	entries       []map[string]any
	once          sync.Once
}

// NewMetricBuffer builds a buffer that delivers through deliver and flushes every interval() (read
// fresh on each tick, so a configuration change takes effect).
func NewMetricBuffer(configuration *Configuration, deliver func([]map[string]any) bool, interval func() time.Duration) *MetricBuffer {
	return &MetricBuffer{configuration: configuration, deliver: deliver, interval: interval}
}

// Record adds one entry (everything but recorded_at, which is stamped here) and reports whether it
// was kept.
func (b *MetricBuffer) Record(entry map[string]any) bool {
	if value, ok := entry["value"].(float64); !ok || math.IsNaN(value) || math.IsInf(value, 0) {
		b.configuration.Logger.Debugf("dropped a metric with a non-finite value")
		return false
	}

	b.once.Do(func() { go b.run() })

	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.entries) >= MaxMetricEntries {
		b.configuration.Logger.Debugf("metric buffer full, dropping a metric")
		return false
	}
	stamped := make(map[string]any, len(entry)+1)
	for k, v := range entry {
		stamped[k] = v
	}
	stamped["recorded_at"] = time.Now().UTC().Format("2006-01-02T15:04:05Z")
	b.entries = append(b.entries, stamped)
	return true
}

// Flush delivers everything buffered so far as one batch. A failed delivery keeps every entry, so
// the next flush's batch just grows.
func (b *MetricBuffer) Flush() {
	b.mu.Lock()
	if len(b.entries) == 0 {
		b.mu.Unlock()
		return
	}
	snapshot := make([]map[string]any, len(b.entries))
	copy(snapshot, b.entries)
	b.mu.Unlock()

	if !b.deliver(snapshot) {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	// Exactly the entries just delivered: anything recorded while the request was in flight sits
	// after them and stays for the next flush.
	b.entries = b.entries[len(snapshot):]
}

func (b *MetricBuffer) run() {
	for {
		interval := b.interval()
		if interval <= 0 {
			interval = time.Minute
		}
		time.Sleep(interval)
		func() {
			// Per tick, so one bad flush never kills the loop.
			defer func() { _ = recover() }()
			b.Flush()
		}()
	}
}

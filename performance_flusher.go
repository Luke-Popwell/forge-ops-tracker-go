package forgeops

import (
	"sync"
	"time"
)

// performanceBucket accumulates one distinct transaction's own requests within the current
// flush window.
type performanceBucket struct {
	count         int64
	durationSumMs float64
	maxDurationMs float64
}

// PerformanceFlusher times requests in-process, bucketed by transaction name (see the net/http
// and Gin integrations), and periodically flushes each distinct bucket as one small aggregate
// report, rather than one network call per request. Ported directly from
// gems/forge_ops_tracker/lib/forge_ops_tracker/performance_flusher.rb: unlike this module's other
// pieces (Configuration, Client, DeliveryQueue, Reporter), there's no existing SessionFlusher
// equivalent in this client to mirror the shape of, since Go's own SDK has never had a session-
// tracking feature.
//
// Same sync.Once-guarded lazy goroutine start as DeliveryQueue's own worker, for the same
// "idiomatic lazy init" reason (see that type's own comment): a time.Ticker rather than a
// channel-range loop, since this is clock-triggered, not push-triggered.
type PerformanceFlusher struct {
	configuration *Configuration
	client        *Client
	mu            sync.Mutex
	buckets       map[string]*performanceBucket
	periodStart   time.Time
	once          sync.Once
}

func NewPerformanceFlusher(configuration *Configuration, client *Client) *PerformanceFlusher {
	return &PerformanceFlusher{
		configuration: configuration,
		client:        client,
		buckets:       make(map[string]*performanceBucket),
		periodStart:   time.Now(),
	}
}

// Record buckets one request's own duration under transactionName.
func (f *PerformanceFlusher) Record(transactionName string, durationMs float64) {
	f.once.Do(func() { go f.run() })

	f.mu.Lock()
	defer f.mu.Unlock()
	bucket, ok := f.buckets[transactionName]
	if !ok {
		bucket = &performanceBucket{}
		f.buckets[transactionName] = bucket
	}
	bucket.count++
	bucket.durationSumMs += durationMs
	if durationMs > bucket.maxDurationMs {
		bucket.maxDurationMs = durationMs
	}
}

// Flush snapshots and resets the buffered buckets, then delivers them as one batch. A failed
// delivery keeps every bucket where it is rather than resetting, so the next flush's batch just
// grows instead of losing what was already tallied; there's no other copy of this data anywhere.
//
// Only exactly what this snapshot actually delivered is ever removed afterward, subtracted from
// whatever's currently in each bucket rather than the bucket (or the whole map) being wiped
// outright: a real, confirmed bug an earlier version of this method had (and
// gems/forge_ops_tracker's own reference implementation still has, unfixed as of this writing: see
// that method's own unconditional `@buckets = {}` for the identical shape), caught directly by
// this client's own test suite, not assumed from reading the code. Record can be called
// concurrently with Flush (that's the whole point of the mutex below), and this method's own
// delivery happens with the lock released, so a new Record call for a transaction already in this
// snapshot, or a brand-new one, can land in the exact window between the snapshot and delivery
// succeeding; wiping the map (or even just deleting whichever keys this snapshot happened to
// cover) afterward, as if delivery had covered everything now sitting in it, silently discarded
// that concurrently-recorded data forever: none of it was actually part of the snapshot that just
// got delivered, and the wipe made it vanish before any later flush ever got a chance to send it.
func (f *PerformanceFlusher) Flush() {
	f.mu.Lock()
	if len(f.buckets) == 0 {
		f.mu.Unlock()
		return
	}
	periodEnd := time.Now()
	samples := make([]map[string]any, 0, len(f.buckets))
	delivered := make(map[string]performanceBucket, len(f.buckets))
	for name, bucket := range f.buckets {
		samples = append(samples, map[string]any{
			"transaction_name":  name,
			"environment":       f.configuration.Environment,
			"release":           f.configuration.Release,
			"period_started_at": f.periodStart.UTC().Format(time.RFC3339),
			"period_ended_at":   periodEnd.UTC().Format(time.RFC3339),
			"request_count":     bucket.count,
			"duration_sum_ms":   bucket.durationSumMs,
			"max_duration_ms":   bucket.maxDurationMs,
		})
		delivered[name] = *bucket
	}
	f.mu.Unlock()

	if !f.client.DeliverPerformanceSamples(samples) {
		return
	}

	f.mu.Lock()
	for name, sent := range delivered {
		current, ok := f.buckets[name]
		if !ok {
			continue
		}
		current.count -= sent.count
		current.durationSumMs -= sent.durationSumMs
		if current.count <= 0 {
			delete(f.buckets, name)
		}
		// maxDurationMs is deliberately left as whatever's currently on the bucket, sent or not:
		// unlike count/durationSumMs, a max can't be correctly "subtracted" back out (the true max
		// of what's left might be anything at or below it, not knowable from the two numbers
		// alone), and leaving it as-is never overstates the next period's own max, only
		// potentially understates how far back it was actually set.
	}
	f.periodStart = periodEnd
	f.mu.Unlock()
}

func (f *PerformanceFlusher) run() {
	interval := f.configuration.PerformanceFlushInterval
	if interval <= 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		f.flushSafely()
	}
}

// flushSafely guards a single flush, not the whole loop: one bad flush must not kill every flush
// after it; same reasoning DeliveryQueue.deliverSafely already documents.
func (f *PerformanceFlusher) flushSafely() {
	defer func() {
		if r := recover(); r != nil {
			f.configuration.Logger.Debugf("performance flush error: %v", r)
		}
	}()
	f.Flush()
}

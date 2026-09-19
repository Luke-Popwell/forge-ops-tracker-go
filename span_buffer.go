package forgeops

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// randomHex returns n random bytes as lowercase hex: 16 for a trace id and 8 for a span id, the
// same sizes gems/forge_ops_tracker's SpanBuffer builds with SecureRandom.hex. The server only
// needs these unique within one trace, never a real database id, so this is deliberately cheap.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is effectively impossible; a constant-length fallback that is still
		// unique enough within one trace beats panicking inside an error reporter's own tracing.
		return hex.EncodeToString([]byte(time.Now().Format("150405.000000000")))[:n*2]
	}
	return hex.EncodeToString(b)
}

// SpanBuffer accumulates one request's own nested call tree in-process; the tracing analog to
// BreadcrumbBuffer's trail. Ported from gems/forge_ops_tracker/lib/forge_ops_tracker/span_buffer.rb
// with the same deliberate departure sdks/node made, for the same reason: the Ruby buffer tracks
// "what's currently open" with one mutable stack, which is wrong once a handler fans work out to
// goroutines that each open their own span (a popped stack entry would leave a sibling
// mis-parented). Here the currently open span travels in the context.Context instead (see
// StartSpan), so every branch gets its own correctly scoped view for free, and this type only holds
// what doesn't depend on that: the trace's identifiers and the flat list of finished spans.
// Guarded by its own mutex for the same reason BreadcrumbBuffer is.
type SpanBuffer struct {
	mu             sync.Mutex
	environment    string
	release        string
	traceID        string
	rootSpanID     string
	spans          []map[string]any
	rootDurationMs *float64
}

func newSpanBuffer(environment, release string) *SpanBuffer {
	return &SpanBuffer{
		environment: environment,
		release:     release,
		traceID:     randomHex(16),
		rootSpanID:  randomHex(8),
	}
}

// record appends one finished span. root is true only for the request's own root span: it forces
// parent_span_id to nil explicitly rather than reading it off "whatever's currently open".
func (b *SpanBuffer) record(spanID, parentSpanID, name, kind string, startedAt time.Time, durationMs float64, data map[string]any, root bool) {
	if data == nil {
		data = map[string]any{}
	}
	var parent any = parentSpanID
	if root || parentSpanID == "" {
		parent = nil
	}
	var release any
	if b.release != "" {
		release = b.release
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.spans = append(b.spans, map[string]any{
		"span_id":        spanID,
		"parent_span_id": parent,
		"name":           name,
		"kind":           normalizeKind(kind),
		// Millisecond precision, exactly the wire format the server expects: a waterfall's own
		// ordering depends on the milliseconds, unlike a breadcrumb's second-precision timestamp.
		"started_at":  startedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		"duration_ms": durationMs,
		"environment": b.environment,
		"release":     release,
		"data":        data,
	})
	if root {
		d := durationMs
		b.rootDurationMs = &d
	}
}

// isSlow is false until the root span has actually been recorded: a request whose integration
// never got the chance to finish it has no duration to compare against a threshold, so it is
// never mistakenly treated as slow.
func (b *SpanBuffer) isSlow(thresholdMs float64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.rootDurationMs != nil && *b.rootDurationMs >= thresholdMs
}

func (b *SpanBuffer) snapshot() []map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]map[string]any, len(b.spans))
	copy(out, b.spans)
	return out
}

type traceContextKey struct{}
type parentSpanContextKey struct{}

func traceFromContext(ctx context.Context) *SpanBuffer {
	if ctx == nil {
		return nil
	}
	buffer, _ := ctx.Value(traceContextKey{}).(*SpanBuffer)
	return buffer
}

func parentSpanFromContext(ctx context.Context, buffer *SpanBuffer) string {
	if parent, ok := ctx.Value(parentSpanContextKey{}).(string); ok && parent != "" {
		return parent
	}
	return buffer.rootSpanID
}

// WithTrace returns a copy of ctx carrying a fresh trace (a new trace id and root span id) that
// StartSpan and RecordSpan attach to, and that FinishTrace later decides whether to send. The
// net/http and Gin Timing middlewares call this on the request's own context, so most host apps
// never call it themselves; call it directly for work that isn't an HTTP request (a queue
// consumer, a cron job). A no-op returning ctx unchanged when Configuration.TrackTracing is off or
// reporting isn't enabled for this environment, so every later call becomes a no-op too.
func WithTrace(ctx context.Context) context.Context {
	config, _ := state()
	if !config.TrackTracing || !config.IsEnabled() {
		return ctx
	}
	buffer := newSpanBuffer(config.Environment, config.Release)
	ctx = context.WithValue(ctx, traceContextKey{}, buffer)
	return context.WithValue(ctx, parentSpanContextKey{}, buffer.rootSpanID)
}

// StartSpan opens a span named name of the given kind ("service" if empty) under whatever span is
// currently open in ctx, and returns a ctx to pass to everything the span covers (so anything
// nested under it, including further spans and RecordSpan calls, is parented to it), plus a func
// that ends it and records it. Idiomatic Go shape, the same one OpenTelemetry's own Go API uses:
//
//	ctx, end := forgeops.StartSpan(ctx, "charge card", "service", nil)
//	defer end()
//
// A no-op (ctx returned unchanged, end does nothing) when ctx carries no trace: never fabricates a
// trace with no request to belong to.
func StartSpan(ctx context.Context, name, kind string, data map[string]any) (context.Context, func()) {
	buffer := traceFromContext(ctx)
	if buffer == nil {
		return ctx, func() {}
	}
	if kind == "" {
		kind = "service"
	}
	spanID := randomHex(8)
	parent := parentSpanFromContext(ctx, buffer)
	startedAt := time.Now()
	ctx = context.WithValue(ctx, parentSpanContextKey{}, spanID)

	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			buffer.record(spanID, parent, name, kind, startedAt, float64(time.Since(startedAt))/float64(time.Millisecond), data, false)
		})
	}
}

// RecordSpan records an already-finished leaf span (a query, an outbound HTTP call) under
// whatever span is currently open in ctx. A no-op when ctx carries no trace. Called by
// Transport below for outbound HTTP; also the way to record a duration you measured yourself.
func RecordSpan(ctx context.Context, name, kind string, startedAt time.Time, duration time.Duration, data map[string]any) {
	buffer := traceFromContext(ctx)
	if buffer == nil {
		return
	}
	buffer.record(randomHex(8), parentSpanFromContext(ctx, buffer), name, kind, startedAt, float64(duration)/float64(time.Millisecond), data, false)
}

// FinishTrace records the request's own root span (kind "controller") once its real total
// duration is finally known, then, only now and entirely client-side, decides whether the whole
// trace reached Configuration.TraceCaptureThreshold and is worth sending at all: a normal, fast
// request's buffer is simply dropped here, unsent, the entire reason this feature costs a fast
// request nothing over the wire. Mirrors gems/forge_ops_tracker's SpanTracing middleware's own
// ensure block. Called by the Timing middlewares; a no-op when ctx carries no trace.
func FinishTrace(ctx context.Context, name string, startedAt time.Time, duration time.Duration) {
	buffer := traceFromContext(ctx)
	if buffer == nil {
		return
	}
	config, queue := spanState()
	buffer.record(buffer.rootSpanID, "", name, "controller", startedAt, float64(duration)/float64(time.Millisecond), nil, true)
	if buffer.isSlow(float64(config.TraceCaptureThreshold) / float64(time.Millisecond)) {
		queue.Push(map[string]any{"trace_id": buffer.traceID, "spans": buffer.snapshot()})
	}
}

// spanKinds are the kinds the ingestion API accepts; anything else would fail validation for the
// whole trace, so an unknown kind is sent as "other" instead.
var spanKinds = map[string]bool{"controller": true, "service": true, "database": true, "redis": true, "http": true, "job": true, "other": true}

func normalizeKind(kind string) string {
	if spanKinds[kind] {
		return kind
	}
	return "other"
}

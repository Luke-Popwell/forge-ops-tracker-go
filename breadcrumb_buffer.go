package forgeops

import (
	"context"
	"sync"
	"time"
)

// Breadcrumb is one entry in a request's trail: what happened, when, and (for the automatic
// controller source) a little structured detail alongside the plain message. Matches the wire
// shape gems/forge_ops_tracker's own BreadcrumbBuffer#add already builds, and the same four keys
// Api::V1::EventsController permits.
type Breadcrumb struct {
	Category  string         `json:"category"`
	Message   string         `json:"message"`
	Level     string         `json:"level"`
	Timestamp string         `json:"timestamp"`
	Data      map[string]any `json:"data"`
}

// BreadcrumbBuffer is a bounded, in-order trail, the same ring-buffer shape (oldest entry dropped
// once full) as every other client in this repo. Guarded by its own mutex rather than relying on
// whatever's holding the *context.Context it's attached to to only ever touch it from one
// goroutine at a time: a handler that fans work out to worker goroutines sharing the same request
// context is a real, unremarkable Go pattern, and each one calling AddBreadcrumb concurrently has
// to be genuinely safe, not just usually fine.
type BreadcrumbBuffer struct {
	mu      sync.Mutex
	maxSize int
	entries []Breadcrumb
}

func newBreadcrumbBuffer(maxSize int) *BreadcrumbBuffer {
	return &BreadcrumbBuffer{maxSize: maxSize}
}

func (b *BreadcrumbBuffer) add(message, category, level string, data map[string]any) {
	if category == "" {
		category = "custom"
	}
	if level == "" {
		level = "info"
	}
	if data == nil {
		data = map[string]any{}
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.maxSize <= 0 {
		return
	}

	b.entries = append(b.entries, Breadcrumb{
		Category:  category,
		Message:   message,
		Level:     level,
		Timestamp: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		Data:      data,
	})
	if overflow := len(b.entries) - b.maxSize; overflow > 0 {
		b.entries = b.entries[overflow:]
	}
}

// all returns a copy of the current trail, safe to hand to a caller (EventBuilder.Build) that
// never holds this buffer's own lock.
func (b *BreadcrumbBuffer) all() []Breadcrumb {
	b.mu.Lock()
	defer b.mu.Unlock()

	out := make([]Breadcrumb, len(b.entries))
	copy(out, b.entries)
	return out
}

// breadcrumbContextKey is an unexported type (not a plain string) so a value this package stores
// via context.WithValue can never collide with a key some unrelated package, or the host app
// itself, happens to also store under the same context: the standard Go idiom for exactly this,
// documented directly on context.WithValue itself.
type breadcrumbContextKey struct{}

// WithBreadcrumbs returns a context carrying a fresh, empty trail for AddBreadcrumb (and the
// net/http/Gin integrations' own automatic controller entry) to accumulate into over the rest of
// this request's lifetime, then CaptureErrorCtx/RecoverCtx to read back and attach to whatever
// gets reported. Package-level, not a method taking an explicit *Configuration, matching Init/
// CaptureError/Recover's own "read the package's own configured singleton" convention: a host app
// installing the net/http or Gin middleware never has to pass its Configuration around by hand for
// this either.
//
// No explicit "clear" step exists here, unlike every thread-pool/worker-reuse client in this repo
// (Ruby's Puma threads, PHP-FPM/Octane's shared-nothing-until-Octane workers, Java's servlet
// thread pool all need one, documented on each of their own breadcrumb middleware): a *http.Request
// gets its own context for exactly one request and nothing else, ever, so there's no later,
// unrelated request that could inherit a stale buffer the way a reused thread or worker process
// could. The buffer simply becomes unreachable, and eligible for garbage collection, the moment
// this request's own context goes out of scope, confirmed directly against how net/http.Server
// derives each request's own context (a fresh child of the server's base context, per Accept, not
// reused across requests) rather than assumed to work the same way Ruby's thread pool does.
func WithBreadcrumbs(ctx context.Context) context.Context {
	config, _ := state()
	if !config.TrackBreadcrumbs || !config.IsEnabled() {
		return ctx
	}
	return context.WithValue(ctx, breadcrumbContextKey{}, newBreadcrumbBuffer(config.MaxBreadcrumbs))
}

// AddBreadcrumb records one entry into whatever trail WithBreadcrumbs already attached to ctx: a
// query, an outbound call, or anything worth remembering right up to the moment something actually
// goes wrong. category defaults to "custom" and level to "info" when left blank, the same
// zero-value-means-default convention Configuration's own env-seeded fields already use elsewhere
// in this client.
//
// A no-op, not a lazily-created buffer, when ctx carries none: unlike Ruby's thread-local (which
// can always lazily create one, since Thread.current is never anyone else's to hand back a new
// value for), a bare context.Context has no way to hand a caller who only has a copy of it back a
// *new* context containing a freshly created buffer; the only context this function was ever given
// is the one already in the caller's hand. Call it from inside a handler wrapped by the net/http or
// Gin middleware (or after your own explicit WithBreadcrumbs call) so a buffer actually exists to
// add to; called from anywhere else, this is a well-defined, harmless no-op, not a panic.
func AddBreadcrumb(ctx context.Context, message, category, level string, data map[string]any) {
	if buffer, ok := ctx.Value(breadcrumbContextKey{}).(*BreadcrumbBuffer); ok {
		buffer.add(message, category, level, data)
	}
}

// breadcrumbsFromContext reads back whatever trail is attached to ctx, or nil if none is (no
// WithBreadcrumbs call ever happened on this context, or TrackBreadcrumbs was off when it did):
// CaptureErrorCtx/RecoverCtx's own internal counterpart to AddBreadcrumb above.
func breadcrumbsFromContext(ctx context.Context) []Breadcrumb {
	if buffer, ok := ctx.Value(breadcrumbContextKey{}).(*BreadcrumbBuffer); ok {
		return buffer.all()
	}
	return nil
}

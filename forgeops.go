// Package forgeops is a Go error reporting client for a ForgeOps tracker
// instance:
//
//	forgeops.Init(func(c *forgeops.Configuration) {
//		c.DSN = "https://<api_key>@your-forgeops-host/api/v1/events"
//	})
//
// See the README for net/http and Gin integration, and what gets captured automatically vs. what
// needs an explicit CaptureError/Recover call. A from-scratch port of gems/forge_ops_tracker (the
// Rails client): see that gem's README for the shared design rationale behind the pieces this
// package is built from (Configuration, EventBuilder, DeliveryQueue, Reporter, Client).
package forgeops

import (
	// Aliased, not a plain "context" import: every function below already has its own
	// map[string]any parameter literally named context (the event's own free-form context data,
	// predating this package's need for context.Context by a long way), which would otherwise
	// shadow the standard library package inside every one of those function bodies and make
	// context.Context/context.Background unreachable exactly where they're needed.
	stdcontext "context"
	"fmt"
	"runtime"
	"sync"
)

var (
	mu                 sync.Mutex
	configuration      *Configuration
	reporter           *Reporter
	performanceFlusher *PerformanceFlusher
)

func state() (*Configuration, *Reporter) {
	mu.Lock()
	defer mu.Unlock()
	ensureConfigurationLocked()
	if reporter == nil {
		client := NewClient(configuration)
		deliveryQueue := NewDeliveryQueue(configuration, client)
		reporter = NewReporter(configuration, NewEventBuilder(configuration), deliveryQueue)
	}
	return configuration, reporter
}

// performanceState mirrors state() above for the performance flusher; kept as its own function
// (locking mu itself) rather than calling state() from inside it, since sync.Mutex isn't
// reentrant: state() locking mu again from within an already-locked performanceState() would
// deadlock the calling goroutine, caught before it shipped rather than assumed safe.
func performanceState() (*Configuration, *PerformanceFlusher) {
	mu.Lock()
	defer mu.Unlock()
	ensureConfigurationLocked()
	if performanceFlusher == nil {
		performanceFlusher = NewPerformanceFlusher(configuration, NewClient(configuration))
	}
	return configuration, performanceFlusher
}

// ensureConfigurationLocked must only be called with mu already held.
func ensureConfigurationLocked() {
	if configuration == nil {
		configuration = NewConfiguration()
	}
}

// Init configures the client. Call once at startup, before http.ListenAndServe or right after
// building your router. Pass a closure to set any Configuration field: the same builder-block
// shape the Java and .NET clients use for the same purpose.
func Init(configure func(*Configuration)) *Configuration {
	config, _ := state()
	if configure != nil {
		configure(config)
	}
	return config
}

// CaptureError reports an error you've already handled. Call it right at the point you'd
// otherwise just log it:
//
//	if err != nil {
//		forgeops.CaptureError(err, nil, nil)
//		return err
//	}
//
// The backtrace is captured right here, at the call site: unlike Python/Java/PHP, a plain Go
// error carries no stack of its own, so CaptureError has to be the one call that knows where the
// trace starts.
//
// user is the affected user, if any (id/email/username, all optional): there's no package-level
// SetUser the way most other clients in this repo have, deliberately. Go has no goroutine-local
// storage at all (no public API for "the current goroutine's identity" exists, specifically to
// discourage this exact pattern), and the idiomatic Go substitute, context.Context propagation,
// would be a real design commitment (threading a context.Context through the net/http and Gin
// integrations' own signatures) that belongs with a future automatic-detection pass for those
// integrations specifically, not a plain package-level mutable variable, which would be a real
// concurrency bug: two goroutines handling different requests at once would stomp on each other's
// value. Pass it explicitly at each call site instead, e.g. from your own middleware/handler.
func CaptureError(err error, context map[string]any, user map[string]any) {
	CaptureErrorCtx(stdcontext.Background(), err, context, user)
}

// CaptureErrorCtx is CaptureError, plus whatever breadcrumb trail WithBreadcrumbs already attached
// to ctx: pass the request's own context (from an http.Request, a gin.Context's Request, or
// anywhere else WithBreadcrumbs was called) so the reported event carries what actually happened
// leading up to it, not just the error itself. CaptureError above is this with
// stdcontext.Background(), which never carries a trail, not a second, independent code path: the
// two stay in sync automatically since one is defined in terms of the other.
func CaptureErrorCtx(ctx stdcontext.Context, err error, context map[string]any, user map[string]any) {
	if err == nil {
		return
	}
	pcs := captureStack()
	_, r := state()
	r.Report(err, context, user, pcs, breadcrumbsFromContext(ctx))
}

// Recover reports a panic and lets it continue unwinding unchanged. Call it deferred, at the top
// of main() or any goroutine you start yourself:
//
//	defer forgeops.Recover(nil, nil)
//
// It never swallows the panic: after reporting, it re-panics with the original value, so whatever
// would have happened without this client: crash the process, a log line from a supervisor, a
// failed test: still happens exactly the same way. The same "report, then don't change program
// behavior" rule the .NET middleware and Python excepthook wrapper both follow. See the
// integrations/nethttp and integrations/gin packages for the request-handler equivalent of this.
//
// user: see CaptureError's own doc for why this is explicit here too, not an ambient setter.
func Recover(context map[string]any, user map[string]any) {
	v := recover()
	if v == nil {
		return
	}
	pcs := captureStack()
	_, r := state()
	r.Report(panicError(v), context, user, pcs, nil)
	panic(v)
}

// RecoverCtx is Recover, plus whatever breadcrumb trail WithBreadcrumbs already attached to ctx:
// same relationship CaptureErrorCtx has to CaptureError above. Deferred exactly the same way:
//
//	defer forgeops.RecoverCtx(ctx, nil, nil)
//
// A plain Recover() call inside this deferred function (rather than duplicating its body) can't
// work here: recover() only ever reports a panic to its own direct caller's defer, so RecoverCtx
// has to call the real runtime recover() itself, not delegate to a function that calls its own.
func RecoverCtx(ctx stdcontext.Context, context map[string]any, user map[string]any) {
	v := recover()
	if v == nil {
		return
	}
	pcs := captureStack()
	_, r := state()
	r.Report(panicError(v), context, user, pcs, breadcrumbsFromContext(ctx))
	panic(v)
}

// RecordPerformance is internal: called by the net/http and Gin integrations, never by host app
// code directly (there's nothing for a caller to decide here beyond what the integration itself
// already measured). Exported (not lowercase) for the same reason PanicError is: a sibling
// integrations/* package, not this one, is what actually calls it.
func RecordPerformance(transactionName string, durationMs float64) {
	config, flusher := performanceState()
	if !config.TrackPerformance || !config.IsEnabled() {
		return
	}
	flusher.Record(transactionName, durationMs)
}

func captureStack() []uintptr {
	pcs := make([]uintptr, MaxFrames+16)
	n := runtime.Callers(1, pcs)
	return pcs[:n]
}

// panicValueError turns whatever was passed to panic() into an error: most panics in idiomatic
// Go already are one, but panic accepts any value.
type panicValueError struct {
	value any
}

func (e panicValueError) Error() string {
	return fmt.Sprintf("%v", e.value)
}

// PanicError turns a recovered panic value into an error, the same conversion Recover uses
// internally: exported for a custom integration (see integrations/gin) that needs to call
// CaptureError directly with a panic value already in hand, without going through Recover's own
// re-panic.
func PanicError(v any) error {
	return panicError(v)
}

func panicError(v any) error {
	if err, ok := v.(error); ok {
		return err
	}
	return panicValueError{value: v}
}

// resetForTesting is not part of the public API: resets package-level state between test cases,
// the same helper every other client's test suite in this repo has its own version of.
func resetForTesting() {
	mu.Lock()
	defer mu.Unlock()
	configuration = nil
	reporter = nil
	performanceFlusher = nil
}

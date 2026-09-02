// Package forgeops is a Go error reporting client for a ForgeOps tracker
// instance:
//
//	forgeops.Init(func(c *forgeops.Configuration) {
//		c.DSN = "https://<api_key>@your-forgeops-host/api/v1/events"
//	})
//
// See the README for net/http and Gin integration, and what gets captured automatically vs. what
// needs an explicit CaptureError/Recover call. A from-scratch port of gems/forge_ops_tracker (the
// Rails client) -- see that gem's README for the shared design rationale behind the pieces this
// package is built from (Configuration, EventBuilder, DeliveryQueue, Reporter, Client).
package forgeops

import (
	"fmt"
	"runtime"
	"sync"
)

var (
	mu            sync.Mutex
	configuration *Configuration
	reporter      *Reporter
)

func state() (*Configuration, *Reporter) {
	mu.Lock()
	defer mu.Unlock()
	if reporter == nil {
		configuration = NewConfiguration()
		client := NewClient(configuration)
		deliveryQueue := NewDeliveryQueue(configuration, client)
		reporter = NewReporter(configuration, NewEventBuilder(configuration), deliveryQueue)
	}
	return configuration, reporter
}

// Init configures the client. Call once at startup, before http.ListenAndServe or right after
// building your router. Pass a closure to set any Configuration field -- the same builder-block
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
//		forgeops.CaptureError(err, nil)
//		return err
//	}
//
// The backtrace is captured right here, at the call site -- unlike Python/Java/PHP, a plain Go
// error carries no stack of its own, so CaptureError has to be the one call that knows where the
// trace starts.
func CaptureError(err error, context map[string]any) {
	if err == nil {
		return
	}
	pcs := captureStack()
	_, r := state()
	r.Report(err, context, pcs)
}

// Recover reports a panic and lets it continue unwinding unchanged. Call it deferred, at the top
// of main() or any goroutine you start yourself:
//
//	defer forgeops.Recover(nil)
//
// It never swallows the panic: after reporting, it re-panics with the original value, so whatever
// would have happened without this client -- crash the process, a log line from a supervisor, a
// failed test -- still happens exactly the same way. The same "report, then don't change program
// behavior" rule the .NET middleware and Python excepthook wrapper both follow. See the
// integrations/nethttp and integrations/gin packages for the request-handler equivalent of this.
func Recover(context map[string]any) {
	v := recover()
	if v == nil {
		return
	}
	pcs := captureStack()
	_, r := state()
	r.Report(panicError(v), context, pcs)
	panic(v)
}

func captureStack() []uintptr {
	pcs := make([]uintptr, MaxFrames+16)
	n := runtime.Callers(1, pcs)
	return pcs[:n]
}

// panicValueError turns whatever was passed to panic() into an error -- most panics in idiomatic
// Go already are one, but panic accepts any value.
type panicValueError struct {
	value any
}

func (e panicValueError) Error() string {
	return fmt.Sprintf("%v", e.value)
}

// PanicError turns a recovered panic value into an error, the same conversion Recover uses
// internally -- exported for a custom integration (see integrations/gin) that needs to call
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

// resetForTesting is not part of the public API -- resets package-level state between test cases,
// the same helper every other client's test suite in this repo has its own version of.
func resetForTesting() {
	mu.Lock()
	defer mu.Unlock()
	configuration = nil
	reporter = nil
}

// Package nethttp wraps a plain net/http.Handler (or anything with the same middleware signature:
// chi, gorilla/mux, and most lightweight routers all compose with a plain
// func(http.Handler) http.Handler) so a panic that escapes it is reported before continuing to
// unwind exactly as it would without this client. Go's own net/http.Server already recovers a
// per-request panic itself (logging it and closing the connection, without crashing the process),
// so re-panicking here is safe and changes nothing about how the server's own handling behaves:
// the same "report, then don't change program behavior" rule every other client in this repo
// follows for its own framework integrations.
package nethttp

import (
	"net/http"
	"time"

	forgeops "github.com/Luke-Popwell/forge-ops-tracker-go"
)

// Middleware wraps next so any panic that escapes a handler is reported with the request's
// method/URL as context, then re-panicked unchanged. Installs a fresh breadcrumb trail on the
// request's own context first (forgeops.WithBreadcrumbs), so AddBreadcrumb calls made anywhere
// inside next, and Timing below's own automatic entry, actually have somewhere to go; reports via
// RecoverCtx rather than Recover so a panic here carries whatever trail accumulated first.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := forgeops.WithBreadcrumbs(r.Context())
		r = r.WithContext(ctx)

		defer forgeops.RecoverCtx(ctx, map[string]any{
			"url":    r.URL.String(),
			"method": r.Method,
		}, nil)
		next.ServeHTTP(w, r)
	})
}

// Timing wraps next so every request's own duration is reported (so a dashboard widget on
// ForgeOps can show which parts of your app are actually slow, not just which ones raise). A
// separate, independent middleware from Middleware above, not layered onto it, since performance
// tracking runs regardless of whether error reporting is even configured: mirrors the same
// several-genuinely-independent-mechanisms split every other client's own integrations already
// use.
//
// The transaction name is "<HTTP method> <request path>", the literal path, not a matched route
// pattern: plain net/http (this module targets Go 1.21, before *http.Request.Pattern existed at
// Go 1.22+) has no route-matching concept of its own to read one from generically, unlike this
// package's Gin sibling (see integrations/gin's own Timing, which uses gin.Context.FullPath()
// instead). A host app whose own router exposes a matched pattern (chi, gorilla/mux) could get
// the same low-cardinality benefit by wrapping this differently; this middleware, wrapping a
// bare http.Handler with no assumptions about which router built it, can't do that on its own.
//
// Also records the same automatic "controller" breadcrumb every other client's own request/
// controller-lifecycle instrumentation already does (message "METHOD path", level "error" on a
// 5xx response and "info" otherwise, data holding the status and path), gated independently on
// forgeops.Configuration.TrackBreadcrumbs the same way this whole rollout's own reference
// (gems/forge_ops_tracker's process_action.action_controller subscription) gates it independently
// of TrackPerformance: install this middleware even with tracking off entirely and AddBreadcrumb
// still has nowhere to go, since Middleware above (not this one) is what actually calls
// forgeops.WithBreadcrumbs; running Timing without Middleware, an unusual but real configuration
// for a host app that wants timing without panic recovery, just makes this one particular
// breadcrumb a no-op along with everything else, not an error.
func Timing(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		// A fresh trace on the request's own context, so StartSpan/RecordSpan/forgeops.Transport
		// calls made anywhere inside next attach to it: see forgeops.WithTrace. Finished below,
		// once the root span's own real duration is known.
		r = r.WithContext(forgeops.WithTrace(r.Context()))
		start := time.Now()
		defer func() {
			duration := time.Since(start)
			durationMs := float64(duration) / float64(time.Millisecond)
			forgeops.RecordPerformance(r.Method+" "+r.URL.Path, durationMs)
			forgeops.FinishTrace(r.Context(), r.Method+" "+r.URL.Path, start, duration)

			level := "info"
			if recorder.status >= 500 {
				level = "error"
			}
			forgeops.AddBreadcrumb(r.Context(), r.Method+" "+r.URL.Path, "controller", level, map[string]any{
				"status": recorder.status,
				"path":   r.URL.Path,
			})
		}()
		next.ServeHTTP(recorder, r)
	})
}

// statusRecorder wraps http.ResponseWriter purely to observe the status code a handler actually
// sent: plain http.ResponseWriter has no getter for it, the standard reason every Go HTTP
// middleware that needs a response's status (logging, metrics, this one) wraps it this way rather
// than there being a built-in way to ask. Defaults to 200 (status set above), matching what
// net/http itself sends when a handler never calls WriteHeader at all.
//
// Known, honest gap for a handler that panics before ever writing anything (composed with
// Middleware above, whose own deferred RecoverCtx is what actually reports the panic): this
// breadcrumb's own deferred func here still runs during the same unwind (Go runs every deferred
// call on the stack as a panic propagates, not just the one that finally recovers it), before
// net/http.Server's real eventual outcome (closing the connection, no response ever sent) is
// knowable from inside this middleware at all, so it records status 200, "info", the same as a
// handler that returned normally, rather than the failure that's actually in flight. Not invented
// to look better than it is: gems/forge_ops_tracker's own reference has the identical shape of gap
// for this same case (Rails' own instrumentation payload carries a nil status when an exception
// propagates through action_controller's usual completion, and that gem's own
// `status && status >= 500 ? "error" : "info"` check treats nil the same as any other falsy value,
// landing on "info" too), not a new inconsistency this port introduces on its own.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

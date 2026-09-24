// Package gin provides a Recovery-style middleware for the Gin web framework. Install it INSTEAD
// OF gin.Recovery(), not alongside it: whichever recovery middleware sits closer to the handler on
// the call stack is the one whose deferred recover() actually sees a panic first, so stacking both
// would leave one of them permanently unused. Gin has no separate hook to subscribe to the way
// Rails' Rack middleware or Spring's @ControllerAdvice do: recovery *is* the framework's error
// handling here, so this middleware has to be it, not observe one from outside.
package gin

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	forgeops "github.com/Luke-Popwell/forge-ops-tracker-go"
)

// Recovery reports any panic that escapes a later handler in the chain, with the request's
// method/URL as context, then aborts the request with a 500: the same response gin.Recovery()
// itself would send, just with the panic also reported first. Installs a fresh breadcrumb trail
// on the request's own context first (forgeops.WithBreadcrumbs), the same reason
// nethttp.Middleware does: so AddBreadcrumb calls made anywhere inside a later handler, and
// Timing below's own automatic entry, have somewhere to go, and CaptureErrorCtx below can report
// whatever trail accumulated first.
//
// Also gives the request its trace context (forgeops.WithRequest, with c.FullPath() as its route):
// the caller's trace when it arrived with a valid traceparent header, a fresh one otherwise. A
// panic reported here, and any error reported with forgeops.CaptureErrorCtx(c.Request.Context(),
// ...) in a later handler, carries the request's trace_id, transaction_name (the same
// "METHOD route" Timing uses) and endpoint ("METHOD /users/:id", left out for a request no route
// matched).
func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request = withRequest(c, forgeops.WithBreadcrumbs(c.Request.Context()))
		ctx := c.Request.Context()

		defer func() {
			if v := recover(); v != nil {
				forgeops.CaptureErrorCtx(ctx, forgeops.PanicError(v), map[string]any{
					"url":    c.Request.URL.String(),
					"method": c.Request.Method,
				}, nil)
				c.AbortWithStatus(http.StatusInternalServerError)
			}
		}()
		c.Next()
	}
}

// Timing reports every request's own duration (so a dashboard widget on ForgeOps can show which
// parts of your app are actually slow, not just which ones raise). Install alongside Recovery
// above; either order still reports the right duration and route, since neither needs to be the
// closest middleware to the handler the way Recovery on its own does, but Recovery first
// (router.Use(Recovery(), Timing())) is the one order where this middleware's own automatic
// breadcrumb below is guaranteed to actually make it into that same panic's own reported event:
// Go still runs every deferred function on the stack during a panic's unwind regardless of which
// one eventually calls recover(), but only entries added *before* whichever deferred call actually
// reports end up in that report. Recovery first means Recovery sits closer to the handler, so its
// own recover() (and the CaptureErrorCtx call inside it) only runs *after* this middleware's own
// deferred breadcrumb has already been added during the same unwind; Timing first would still add
// the breadcrumb, just one step too late to ever be seen by the report that already went out.
//
// The transaction name is "<HTTP method> <route pattern>" (e.g. "GET /users/:id"), not the raw
// URL: c.FullPath() is Gin's own matched route pattern, set once routing succeeds, which keeps a
// distinct user id from exploding into its own separate transaction the way the literal path
// would. Falls back to c.Request.URL.Path when FullPath() returns empty (a request no route
// matched at all, a 404).
//
// Also records the same automatic "controller" breadcrumb nethttp.Timing does, on the identical
// gate/shape: message "METHOD route", level "error" on a 5xx response and "info" otherwise, data
// holding the status and route. c.Writer.Status() is Gin's own already-tracked response status
// (set by its ResponseWriter wrapper before this middleware ever sees it), so unlike the plain
// net/http integration, nothing extra needs wrapping here to observe it.
//
// Deferred, not plain code after c.Next() the way this middleware's own pre-breadcrumb version
// had it (confirmed directly, not assumed: a real, reproducible bug this rollout's own test suite
// caught, not just a hypothetical): c.Next() never returns at all when a later handler panics, a
// panic instead propagates straight up through this stack frame to whatever recovers it first
// (Recovery above), skipping every line after a bare c.Next() call entirely. Both RecordPerformance
// and AddBreadcrumb need to fire during that unwind too, exactly like nethttp.Timing's own deferred
// version already correctly does, or a panicking request (the one case a breadcrumb trail is
// actually worth having) would silently never get either.
//
// The trace continues the caller's when the request arrived with a valid traceparent header (see
// forgeops.WithRequest), and is sent when it was slow or when the request errored: an error
// reported with its context, or a panic passing through here on its way out.
func Timing() gin.HandlerFunc {
	return func(c *gin.Context) {
		// The request's trace context (a no-op if Recovery already gave it one), then a trace on
		// it: see forgeops.WithTrace. Finished below, once the root span's own real duration is
		// known.
		c.Request = withRequest(c, c.Request.Context())
		c.Request = c.Request.WithContext(forgeops.WithTrace(c.Request.Context()))
		start := time.Now()
		completed := false
		defer func() {
			if !completed {
				// A later handler panicked: only observed here on its way out.
				forgeops.MarkRequestErrored(c.Request.Context())
			}
			duration := time.Since(start)
			durationMs := float64(duration) / float64(time.Millisecond)
			route := c.FullPath()
			if route == "" {
				route = c.Request.URL.Path
			}
			forgeops.RecordPerformance(c.Request.Method+" "+route, durationMs)
			forgeops.FinishTrace(c.Request.Context(), c.Request.Method+" "+route, start, duration)

			level := "info"
			status := c.Writer.Status()
			if status >= 500 {
				level = "error"
			}
			forgeops.AddBreadcrumb(c.Request.Context(), c.Request.Method+" "+route, "controller", level, map[string]any{
				"status": status,
				"path":   route,
			})
		}()
		c.Next()
		completed = true
	}
}

// withRequest returns c.Request with ctx and the request's trace context (see forgeops.WithRequest),
// its route set to Gin's matched route pattern: Gin routes before any handler in the chain runs, so
// c.FullPath() is already known here, and an error reported from the handler itself has it.
func withRequest(c *gin.Context, ctx context.Context) *http.Request {
	r := forgeops.WithRequest(c.Request.WithContext(ctx))
	forgeops.SetRequestRoute(r.Context(), c.FullPath())
	return r
}

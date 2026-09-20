# tracker-go

Go error reporting client for [ForgeOps](https://getforgeops.net). Requires
Go 1.21+. It captures panics and reported `error` values, builds a backtrace, scrubs likely PII, and
delivers events to ForgeOps over HTTP without blocking the caller.

## Installation

```
go get github.com/Luke-Popwell/forge-ops-tracker-go
```

This is a mirror, kept in sync automatically from `sdks/go` in the main `forge_ops` repo (which
is private, so isn't itself something `go get` could ever fetch directly): develop against that
repo, not this one.

The Gin integration (`integrations/gin`) is its own Go module with its own `go.mod`, so pulling in
Gin and its dependency tree only happens if you actually import that subpackage: the root module
stays dependency-free, built entirely on the standard library.

## Configuration

Set a DSN (from a project's settings page in ForgeOps), either via the `FORGE_OPS_DSN` environment
variable or explicitly:

```go
import forgeops "github.com/Luke-Popwell/forge-ops-tracker-go"

forgeops.Init(func(c *forgeops.Configuration) {
    c.DSN = "https://<api_key>@getforgeops.net/api/v1/events" // or leave unset to read FORGE_OPS_DSN
    c.Release = "..."
    c.Environment = "production"
})
```

Call `Init` once at startup, before `http.ListenAndServe` or right after building your router. Pass
a closure to set any `Configuration` field, so every option is available through the one call
without a long list of positional arguments or a separate setter for each field.

### Plain net/http (and anything with the same middleware signature: chi, gorilla/mux, most
### lightweight routers)

```go
import (
    "net/http"

    forgeops "github.com/Luke-Popwell/forge-ops-tracker-go"
    forgeopshttp "github.com/Luke-Popwell/forge-ops-tracker-go/integrations/nethttp"
)

mux := http.NewServeMux()
mux.HandleFunc("/orders", ordersHandler)
http.ListenAndServe(":8080", forgeopshttp.Middleware(forgeopshttp.Timing(mux)))
```

### Gin

```go
import (
    "github.com/gin-gonic/gin"

    forgeopsgin "github.com/Luke-Popwell/forge-ops-tracker-go/integrations/gin"
)

router := gin.New()
router.Use(forgeopsgin.Recovery()) // instead of gin.Recovery(), not alongside it
router.Use(forgeopsgin.Timing())   // either order relative to Recovery is fine
```

## What gets reported automatically, and what doesn't

**A panic that escapes a request handler needs no further wiring at all**, once the relevant
middleware above is installed: `net/http`'s own server already recovers a per-request panic by
closing the connection (rather than crashing the process); the middleware reports it first, then
lets that happen exactly as it would without this client. Gin has no separate recovery hook to
observe: `forgeopsgin.Recovery()` has to *be* the recovery, which is why it's a drop-in
replacement for `gin.Recovery()`, not an addition alongside it.

**Go doesn't have exceptions**, so there's no single language-level hook that reports "whatever
wasn't handled." Two entry points cover the two shapes Go errors actually come in:

```go
// An error you already have: report it right where you'd otherwise just log it:
if err != nil {
    forgeops.CaptureError(err, map[string]any{"order_id": order.ID}, nil)
    return err
}

// A panic in a goroutine you started yourself (main(), a worker loop, anything not already
// covered by the net/http/Gin middleware above): defer this at the top:
func worker() {
    defer forgeops.Recover(nil, nil)
    // ...
}
```

`Recover` never swallows the panic: after reporting, it re-panics with the original value, so
whatever would have happened without this client: crash the process, a log line from a
supervisor, a failed test: still happens exactly the same way. Reporting an error should never
change what your program actually does.

A plain Go `error` carries no stack trace of its own, so `CaptureError`/`Recover` capture the
backtrace at their own call site via `runtime.Callers`, not from the error value. Call
`CaptureError` as close to the point you learned about the error as you reasonably can, for the
most useful trace.

Delivery happens on a background goroutine with a bounded channel and a short per-request HTTP
timeout (`Configuration.Timeout`, 2s default). Every failure mode: network errors, timeouts, a
full queue, a malformed DSN: is caught and dropped rather than propagated, so a broken or
unreachable tracker can never take down the host app.

## Identifying users

```go
forgeops.CaptureError(err, nil, map[string]any{"id": user.ID, "email": user.Email})
```

Unlike most other clients in this repo, there's no package-level `SetUser`: Go has no
goroutine-local storage at all (no public API for "the current goroutine's identity" exists,
specifically to discourage this exact pattern), and the idiomatic Go substitute,
`context.Context` propagation, would be a real design commitment (threading a `context.Context`
through the net/http and Gin middleware's own signatures) that belongs with a future automatic
auth-detection pass for those integrations specifically, not a plain package-level mutable
variable, which would be a real concurrency bug: two goroutines handling different requests at
once would stomp on each other's value. Pass the user explicitly at each call site instead, e.g.
from your own middleware/handler, the same way `context` (the free-text map, not
`context.Context`) already works. Shows up on an issue's own detail page, and as its own
affected-users count alongside the regular event count.

## `in_app` backtrace frames

A Go binary built without `-trimpath` embeds the real build-time source paths, so file-path
matching against `Configuration.AppRoot` is a straightforward prefix comparison against those
embedded paths. Defaults to the current working directory; set it explicitly if that doesn't match
your binary's actual build layout. Standard-library frames (`runtime.GOROOT()`) and third-party
dependency frames (anything under the module cache's `/pkg/mod/`) are never marked `in_app`,
regardless of `AppRoot`.

## PII scrubbing

By default, the message, backtrace, and any context you attach are scanned for likely personal
data (email addresses, formatted SSNs/credit cards, known API key/token formats, and anything
under a suspiciously-named key like `password`, `api_key`, or `ssn`) and redacted before
the payload ever leaves this process. ForgeOps itself scrubs again on arrival regardless, so this
is a second, earlier layer, not the only one. The user attached via `CaptureError`/`Recover`'s
`user` parameter above is a deliberate exception: it's never scrubbed, since redacting it would
defeat the whole point of identifying users in the first place.

To disable it:

```go
forgeops.Init(func(c *forgeops.Configuration) {
    c.ScrubPII = false
})
```

## Distributed tracing

Each request gets its own trace: a root span for the request plus a tree of spans nested under it
(service work you wrap yourself, outbound HTTP calls, anything else you record). **Only a slow
request's trace is ever sent**: once the request finishes and its real duration is known, the whole
trace goes out only if it reached `TraceCaptureThreshold` (1 second by default); a fast request's
spans are dropped, so it costs nothing over the wire. On by default; turn it off with
`TrackTracing = false`.

The `nethttp.Timing` and `gin.Timing` middlewares open the trace on the request's own context and
finish it for you (the root span is named like the performance transaction: the matched route
pattern under Gin, the literal path under plain net/http). Inside a handler:

```go
ctx, end := forgeops.StartSpan(r.Context(), "charge card", "service", map[string]any{"order_id": id})
defer end()
// pass ctx to everything the span covers: anything opened from it nests under "charge card"
```

The open span travels in the `context.Context`, not on a shared stack, so goroutines that each open
a span from the same context each parent correctly (to the request root), and a child opened inside
one parents to that one, whatever order they interleave in (a test pins this).

Outbound HTTP is recorded as an `http` span, named `METHOD host` (never the path or query, which
could carry an id or a token), if you make the call with a trace-carrying context through the
wrapped transport:

```go
client := &http.Client{Transport: forgeops.Transport(nil)}
req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
resp, err := client.Do(req)
```

`Transport` never changes the request, the response, or the error. A request made with a bare
`context.Background()` records nothing, and every call is a no-op when the context carries no trace.
For work that isn't an HTTP request (a queue consumer, a cron job), call `forgeops.WithTrace(ctx)` at
the start and `forgeops.FinishTrace(ctx, name, startedAt, duration)` at the end yourself.

Delivery is on its own small goroutine and bounded queue (one trace per POST to `/spans`), so
reporting a slow request never makes it slower; a full queue drops the trace rather than blocking.

## Source context

By default, each in-app backtrace frame (never a standard-library or module-cache frame) is
captured along with the 5 lines of source on either side of the culprit line, read straight off
disk at capture-time, so an issue's detail page can show the actual code that broke, not just a
`file:line:method` reference. This never applies to a frame outside your configured `AppRoot`, and
it fails silently (no context, not a panic) for any file that can't be read for whatever reason.

This is a real, deliberate exception to "off by default is safer": literal source code is being
transmitted, not just a reference to it, and the real protection here is not this field. Every
project on ForgeOps has its own setting (on by default, off durably and immediately once an org
owner turns it off, regardless of what any individual app's own `CaptureSourceContext` is still set
to) that governs whether the server will ever actually store what a client sends. Set this to
`false` if you'd rather this client never even attempt the disk read in the first place:

```go
forgeops.Init(func(c *forgeops.Configuration) {
    c.CaptureSourceContext = false
})
```

## Performance monitoring

By default, `forgeopshttp.Timing`/`forgeopsgin.Timing()` also time every request, so a dashboard
widget on ForgeOps can show which parts of your app are actually slow, not just which ones raise.
Bucketed by transaction and flushed as a small periodic aggregate per transaction on a
`time.Ticker` (never one network call per request), the same delivery philosophy as everything
else in this client. Under Gin, the transaction is `"<HTTP method> <route pattern>"` (e.g.
`"GET /users/:id"`, `gin.Context.FullPath()`'s own matched pattern, not the raw path, so a
distinct user id doesn't explode into its own separate transaction). Plain `net/http` has no
route-matching concept of its own at this module's Go version floor to read a pattern from
generically, so `forgeopshttp.Timing` reports the literal request path instead; a host app using
chi or gorilla/mux could get the same low-cardinality benefit by wrapping this differently.

Each aggregate also carries a small latency histogram (a count per fixed latency bucket: 50, 100,
250, 500, 1000, 2500, 5000 and 10000ms, plus an overflow bucket), so ForgeOps can show an
approximate p50/p95/p99 per transaction, not just an average. Percentiles are accurate to the width
of whichever bucket a duration falls into; the SDK never stores the individual durations.

```go
forgeops.Init(func(c *forgeops.Configuration) {
    c.TrackPerformance = false                        // opt out entirely
    c.PerformanceFlushInterval = 30 * time.Second      // default 60 seconds
})
```

Requires a ForgeOps plan that includes performance monitoring; on a plan that doesn't, the
periodic flushes are simply rejected server-side and dropped, exactly like any other delivery
failure.

## Custom metrics and infrastructure monitoring

Two explicit calls (nothing is automatic, so there is no `Track*` flag): a business event you name
yourself, and a reading from one of your own hosts.

```go
forgeops.CaptureMetric("signup", 1)    // a bare counter
forgeops.CaptureMetric("payment", 49)  // a real magnitude; it may be negative (a refund)

forgeops.CaptureInfrastructureMetric("cpu", 0.42, "")      // "" defaults to Configuration.ServerName
forgeops.CaptureInfrastructureMetric("disk", 0.81, "db-1")
forgeops.FlushMetrics()                                    // send right now
```

Each capture is buffered and flushed as one batch every `MetricFlushInterval` /
`InfrastructureMetricFlushInterval` (60 seconds by default) on a goroutine started on the first
capture. **Go has no exit hook to flush from**, so a short-lived program (a cron job) must call
`forgeops.FlushMetrics()` before it returns from `main`, typically `defer`red right after `Init`.
Every entry is stored as it was captured (a signup is a row, not a running total), so a count or sum
you compute later is exact. Both are a no-op when the client isn't enabled for the environment.

A failed delivery keeps every entry for the next flush, and an entry captured while a delivery is in
flight is kept too (the Ruby gem's own buffer loses it; a `-race` test pins this with a gated
delivery). The buffer holds at most 1000 entries per kind and drops further ones until a flush
succeeds, since a plan without the feature rejects every flush and would otherwise grow it for as
long as the process lives. A NaN or infinite value is dropped at capture: `encoding/json` refuses to
marshal one, which would make every batch behind it fail to send. Requires a ForgeOps plan that
includes custom metrics / infrastructure monitoring.

## Breadcrumbs

A bounded, ordered trail of what happened right before an error: SQL/query calls this client has
no query-level instrumentation of its own to source automatically, so only the request/controller
lifecycle is recorded for you, plus anything added by hand. On by default, capped at the 30 most
recent entries per request, both configurable:

```go
forgeops.Init(func(c *forgeops.Configuration) {
    c.TrackBreadcrumbs = false // opt out entirely
    c.MaxBreadcrumbs = 50      // default 30
})
```

Unlike every other client in this repo, there's no package-level `AddBreadcrumb`-only-needs-a-
message call and no thread-local/goroutine-local trail: Go has no such mechanism (the same reason
`CaptureError`/`Recover` take an explicit `user` argument rather than a package-level `SetUser`),
so a trail lives on a `context.Context` instead, installed once per request:

```go
// net/http
router.Handle("/orders", forgeopshttp.Middleware(forgeopshttp.Timing(yourHandler)))

// Gin (Middleware first: see forgeopsgin.Timing's own doc comment for why)
router.Use(forgeopsgin.Recovery(), forgeopsgin.Timing())
```

`forgeopshttp.Middleware`/`forgeopsgin.Recovery` are what actually install the trail
(`forgeops.WithBreadcrumbs`) on the request's own context; `Timing` in either package adds one
automatic entry to it per request (category `"controller"`, level `"error"` on a 5xx response),
gated on `TrackBreadcrumbs` independently of `TrackPerformance`, the same "several genuinely
independent mechanisms" pattern this client's own performance monitoring already follows. Add your
own from inside a handler wrapped by either:

```go
forgeops.AddBreadcrumb(r.Context(), "charged card", "custom", "info", map[string]any{"order_id": order.ID})
```

Then report through the same context so whatever accumulated actually gets attached:

```go
forgeops.CaptureErrorCtx(r.Context(), err, nil, nil)
```

`CaptureError`/`Recover` (no `Ctx` suffix) still work exactly as before, and never carry a trail:
they're `CaptureErrorCtx`/`RecoverCtx` called with `context.Background()`, which never has one
attached. `AddBreadcrumb` called on a context nobody ever installed a trail on (no `Middleware`/
`Recovery`, or `TrackBreadcrumbs` off when they ran) is a well-defined, harmless no-op, not a panic.

## Running the tests

```bash
cd sdks/go
go test ./...
go vet ./...
gofmt -l .   # should print nothing

cd integrations/gin
go test ./...
```

# tracker-go

Go error reporting client for a private, self-hosted [ForgeOps](../../) tracker instance. Requires
Go 1.21+. A from-scratch port of [`gems/forge_ops_tracker`](../../gems/forge_ops_tracker) (the
Rails client) -- see that gem's README for the shared design rationale; this document only covers
what's Go-specific.

## Installation

```
go get github.com/Luke-Popwell/forge-ops-tracker-go
```

This is a mirror, kept in sync automatically from `sdks/go` in the main `forge_ops` repo (which
is private, so isn't itself something `go get` could ever fetch directly) -- develop against that
repo, not this one.

The Gin integration (`integrations/gin`) is its own Go module with its own `go.mod`, so pulling in
Gin and its dependency tree only happens if you actually import that subpackage -- the root module
stays dependency-free (standard library only), the same "zero runtime dependencies" property the
Perl and Python clients have.

## Configuration

Set a DSN (from a project's settings page in ForgeOps), either via the `FORGE_OPS_DSN` environment
variable or explicitly:

```go
import forgeops "github.com/Luke-Popwell/forge-ops-tracker-go"

forgeops.Init(func(c *forgeops.Configuration) {
    c.DSN = "https://<api_key>@your-forgeops-host/api/v1/events" // or leave unset to read FORGE_OPS_DSN
    c.Release = "..."
    c.Environment = "production"
})
```

Call `Init` once at startup, before `http.ListenAndServe` or right after building your router. Pass
a closure to set any `Configuration` field -- the same builder-block shape the Java and .NET
clients use for their own `init`/`AddForgeOpsTracker` call.

### Plain net/http (and anything with the same middleware signature -- chi, gorilla/mux, most
### lightweight routers)

```go
import (
    "net/http"

    forgeops "github.com/Luke-Popwell/forge-ops-tracker-go"
    forgeopshttp "github.com/Luke-Popwell/forge-ops-tracker-go/integrations/nethttp"
)

mux := http.NewServeMux()
mux.HandleFunc("/orders", ordersHandler)
http.ListenAndServe(":8080", forgeopshttp.Middleware(mux))
```

### Gin

```go
import (
    "github.com/gin-gonic/gin"

    forgeopsgin "github.com/Luke-Popwell/forge-ops-tracker-go/integrations/gin"
)

router := gin.New()
router.Use(forgeopsgin.Recovery()) // instead of gin.Recovery(), not alongside it
```

## What gets reported automatically, and what doesn't

**A panic that escapes a request handler needs no further wiring at all**, once the relevant
middleware above is installed -- `net/http`'s own server already recovers a per-request panic by
closing the connection (rather than crashing the process); the middleware reports it first, then
lets that happen exactly as it would without this client. Gin has no separate recovery hook to
observe -- `forgeopsgin.Recovery()` has to *be* the recovery, which is why it's a drop-in
replacement for `gin.Recovery()`, not an addition alongside it.

**Go doesn't have exceptions**, so unlike the Ruby/Python/Java/PHP/Node clients, there's no single
language-level hook that reports "whatever wasn't handled." Two entry points cover the two shapes
Go errors actually come in:

```go
// An error you already have -- report it right where you'd otherwise just log it:
if err != nil {
    forgeops.CaptureError(err, map[string]any{"order_id": order.ID})
    return err
}

// A panic in a goroutine you started yourself (main(), a worker loop, anything not already
// covered by the net/http/Gin middleware above) -- defer this at the top:
func worker() {
    defer forgeops.Recover(nil)
    // ...
}
```

`Recover` never swallows the panic: after reporting, it re-panics with the original value, so
whatever would have happened without this client -- crash the process, a log line from a
supervisor, a failed test -- still happens exactly the same way. The same "report, then don't
change program behavior" rule the .NET middleware and Python `excepthook` wrapper both follow.

A plain Go `error` carries no stack trace of its own (unlike Python's traceback or Java's
`Throwable`, which travel with the exception), so `CaptureError`/`Recover` capture the backtrace
at their own call site via `runtime.Callers`, not from the error value. Call `CaptureError` as
close to the point you learned about the error as you reasonably can, for the most useful trace.

Delivery happens on a background goroutine with a bounded channel and a short per-request HTTP
timeout (`Configuration.Timeout`, 2s default). Every failure mode -- network errors, timeouts, a
full queue, a malformed DSN -- is caught and dropped rather than propagated, so a broken or
unreachable tracker can never take down the host app.

## `in_app` backtrace frames

A Go binary built without `-trimpath` embeds the real build-time source paths, so file-path
matching against `Configuration.AppRoot` works the same way the Ruby gem's `Rails.root` comparison
and the Python client's `os.getcwd()` comparison do. Defaults to the current working directory; set
it explicitly if that doesn't match your binary's actual build layout. Standard-library frames
(`runtime.GOROOT()`) and third-party dependency frames (anything under the module cache's
`/pkg/mod/`) are never marked `in_app`, regardless of `AppRoot`.

## PII scrubbing

Same behavior as every other client in this repo: the message, backtrace, and any context you
attach are scanned for likely personal data -- email addresses, formatted SSNs/credit cards, known
API key/token formats, and anything under a suspiciously-named key (`password`, `api_key`, `ssn`,
and similar) -- and redacted before the payload ever leaves this process. ForgeOps itself scrubs
again on arrival regardless, so this is a second, earlier layer, not the only one.

To disable it:

```go
forgeops.Init(func(c *forgeops.Configuration) {
    c.ScrubPII = false
})
```

## Running the tests

```bash
cd sdks/go
go test ./...
go vet ./...
gofmt -l .   # should print nothing

cd integrations/gin
go test ./...
```

# Changelog

## 0.6.0 (2026-09-25)

- New `forgeops.RecordChange(kind, title, options)` records a change that isn't a deploy (a feature flag, a config edit, a migration, a dependency or infrastructure change) so it shows up alongside errors and performance data. `kind` is one of the new `ChangeKind*` constants, anything else being sent as `other`; `ChangeOptions` carries the optional `Details`, `Environment` (defaulting to `Configuration.Environment`), `Service`, `Actor`, `URL`, `ID` (an idempotency key) and `OccurredAt` (defaulting to now). Delivered on the existing background delivery goroutine; never blocks, never panics, and is a no-op when the client isn't enabled.
- The first `Init` that enables the client now sends one change snapshot per process, in the background: the Go version, plus every module version compiled into the binary (from `debug.ReadBuildInfo`). ForgeOps diffs it against the previous snapshot for the same environment to record what changed between deploys. New `Configuration.DetectChanges` (default true) turns it off.
- New `Configuration.TrackEnvVarNames` (default false) adds the names, never the values, of the process's environment variables to that snapshot, leaving out host-specific names (`HOSTNAME`, `PATH`, `LC_*`, `KUBERNETES_*`, Kubernetes service variables and similar) and this client's own `FORGE_OPS_*` variables.
- The Gin integration needs no update and still requires v0.5.0 of this module; it uses none of the above.

## 0.5.0

- Distributed tracing across services, using the W3C Trace Context standard (`traceparent`). `nethttp.Middleware`, `nethttp.Timing`, `gin.Recovery` and `gin.Timing` now give every request a trace id on its context (new `forgeops.WithRequest(r)`, for any other router's middleware); one that arrives with a valid `traceparent` header continues that trace, and its root span points at the caller's span. A missing or malformed header starts a fresh trace. `forgeops.TraceID(ctx)` returns it.
- Every error reported with a request's context (a panic the middleware recovers, or `CaptureErrorCtx`/`RecoverCtx`) now says where it happened: `trace_id`, `transaction_name` (the same name performance samples and traces already use), and `endpoint` (the HTTP method plus Gin's matched route, or the `http.ServeMux` pattern that matched on Go 1.22+; never the literal path). New `forgeops.SetRequestRoute(ctx, pattern)` supplies the route from any other router. Read when the error is reported, so an error reported from inside a handler already has them. Left out outside a request (`trace_id` aside, inside a `WithTrace` trace). Structured fields, so they're never PII-scrubbed.
- `forgeops.Transport` now sends a `traceparent` header on outbound calls made with a request's (or `WithTrace` trace's) context, whose parent id is that call's own `http` span. The header goes on a clone, so the request you pass in is never modified; a `traceparent` you set yourself is never replaced, and this client's own requests to ForgeOps never carry one. New `Configuration.PropagateTraces` (default true) and `Configuration.TracePropagationTargets` (default nil, meaning every host; or a `[]any` of host strings, each matching that host and its subdomains, and/or `*regexp.Regexp`s) control where the header goes.
- An errored request's trace is always sent, however fast it was: an error reported with its context, or a panic passing through `Timing` (new `forgeops.MarkRequestErrored(ctx)` for other integrations). Fast, successful requests are unchanged.
- With `TrackTracing = false`, requests still get a trace id (attached to errors and propagated in the header, since that is also what links errors across services); only span reporting stops.
- Trace and span ids were already W3C shaped (32 and 16 lowercase hex characters); they are now also guaranteed never to be all zeros, the one value the standard reserves as invalid.

This client is versioned by git tags on its public mirror (the last one was v0.4.0), not by a version
file, and no `CHANGELOG.md` existed for it before this entry; it starts here rather than
backfilling every earlier version.

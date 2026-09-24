package forgeops

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
)

// requestState is everything this client knows about the current web request that isn't already
// carried by the older, single-purpose context values (the breadcrumb trail, the span buffer):
// which trace it belongs to, which remote span called it (if any), what it's called, and whether
// it errored. Mirrors gems/forge_ops_tracker's own RequestState. Lives on the request's
// context.Context (see WithRequest), so CaptureErrorCtx, RecoverCtx, Transport and the request's
// own trace all agree on one trace id.
//
// The trace id exists for every request, not only when span tracing is on: it's also what an
// error event carries so ForgeOps can link it to errors reported by other services taking part in
// the same trace.
type requestState struct {
	traceID      string
	parentSpanID string
	method       string
	path         string
	errored      atomic.Bool

	mu sync.Mutex
	// request is the latest *http.Request WithRequest handed out for this request: the one that
	// reaches the router, and so the one Go 1.22+'s ServeMux fills in the matched Pattern on.
	request *http.Request
	// route is the framework's matched route template, when an integration knows it (see
	// SetRequestRoute); it wins over request's Pattern.
	route string
}

type requestStateKey struct{}

func requestFromContext(ctx context.Context) *requestState {
	if ctx == nil {
		return nil
	}
	state, _ := ctx.Value(requestStateKey{}).(*requestState)
	return state
}

// WithRequest returns a copy of r whose context carries this request's trace context: the
// caller's trace when r arrived with a valid traceparent header (same trace id, and the caller's
// span as this request's remote parent), a fresh W3C trace id otherwise. Errors reported with that
// context (CaptureErrorCtx, RecoverCtx) then carry the request's trace_id, transaction_name and
// endpoint, and outbound calls made with it through Transport carry a traceparent header. The
// net/http and Gin integrations call this for you; call it yourself from your own middleware for
// any other router, and SetRequestRoute with its matched route.
//
// Safe to call from several middlewares on the same request: the first call creates the trace
// context, later ones keep it (returning r itself) and only remember r as the request the router
// will see, since Go 1.22+'s ServeMux records the matched pattern on exactly that request. Returns
// r unchanged when reporting isn't enabled for this environment.
func WithRequest(r *http.Request) *http.Request {
	if r == nil {
		return r
	}
	if state := requestFromContext(r.Context()); state != nil {
		state.bind(r)
		return r
	}
	config, _ := state()
	if !config.IsEnabled() {
		return r
	}

	state := &requestState{method: r.Method}
	if r.URL != nil {
		state.path = r.URL.Path
	}
	if traceID, parentSpanID, ok := parseTraceParent(r.Header.Get(TraceParentHeader)); ok {
		state.traceID, state.parentSpanID = traceID, parentSpanID
	} else {
		state.traceID = generateTraceID()
	}
	r = r.WithContext(context.WithValue(r.Context(), requestStateKey{}, state))
	state.bind(r)
	return r
}

// SetRequestRoute records the framework's matched route template for the request ctx belongs to
// (e.g. "/users/:id" or "/users/{id}", never the literal path): the endpoint reported with its
// errors becomes "<METHOD> <route>", and the transaction name uses it too. The Gin integration
// calls this with c.FullPath(); from another router's middleware, pass its own matched pattern
// (chi's RouteContext(ctx).RoutePattern(), for example). A no-op outside a request or for "".
func SetRequestRoute(ctx context.Context, route string) {
	state := requestFromContext(ctx)
	if state == nil || route == "" {
		return
	}
	state.mu.Lock()
	state.route = route
	state.mu.Unlock()
}

// TraceID returns the id of the trace ctx belongs to (32 lowercase hex characters): the web
// request's own when ctx carries one (see WithRequest), even with TrackTracing off, or a trace
// started with WithTrace; "" outside both. Handy for your own logs.
func TraceID(ctx context.Context) string {
	if state := requestFromContext(ctx); state != nil {
		return state.traceID
	}
	if buffer := traceFromContext(ctx); buffer != nil {
		return buffer.traceID
	}
	return ""
}

func (s *requestState) bind(r *http.Request) {
	s.mu.Lock()
	s.request = r
	s.mu.Unlock()
}

// names returns the request's transaction name (the same name performance samples and the root
// span use: the route SetRequestRoute recorded, else the literal path) and its endpoint (the route
// template, from SetRequestRoute or ServeMux's matched pattern; "" when neither is known, never the
// literal path). Read when an error is reported, not when the request starts, since the route is
// only known once the router has matched it.
func (s *requestState) names() (transactionName, endpoint string) {
	s.mu.Lock()
	route, request := s.route, s.request
	s.mu.Unlock()

	transactionName = s.method + " " + s.path
	if route != "" {
		transactionName = s.method + " " + route
	} else if request != nil {
		route = servePatternPath(request)
	}
	if route != "" {
		endpoint = s.method + " " + route
	}
	return transactionName, endpoint
}

var (
	patternFieldOnce  sync.Once
	patternFieldIndex []int
)

// servePatternPath reads the route pattern Go 1.22+'s ServeMux records on the request it matched
// (http.Request.Pattern, e.g. "GET /orders/{id}"), keeping only its path: the method is reported
// separately, and a host-specific pattern's host isn't part of the route. Read by reflection so
// this module still builds on Go 1.21, whose http.Request has no such field (nor does a request no
// ServeMux ever routed, or one routed with GODEBUG=httpmuxgo121=1): "" in every such case.
func servePatternPath(r *http.Request) string {
	patternFieldOnce.Do(func() {
		if field, ok := reflect.TypeOf((*http.Request)(nil)).Elem().FieldByName("Pattern"); ok && field.Type.Kind() == reflect.String {
			patternFieldIndex = field.Index
		}
	})
	if patternFieldIndex == nil {
		return ""
	}
	pattern := reflect.ValueOf(r).Elem().FieldByIndex(patternFieldIndex).String()
	if i := strings.IndexByte(pattern, ' '); i >= 0 {
		pattern = strings.TrimSpace(pattern[i+1:])
	}
	if i := strings.IndexByte(pattern, '/'); i >= 0 {
		return pattern[i:]
	}
	return ""
}

// traceFields is where an error happened: the request's (or trace's) id, transaction name and
// endpoint, each "" when unknown.
type traceFields struct {
	traceID         string
	transactionName string
	endpoint        string
}

// traceFieldsForError reads the fields an error reported with ctx should carry, marking the
// request as errored on the way (so FinishTrace sends its trace however fast it was).
func traceFieldsForError(ctx context.Context) traceFields {
	if state := requestFromContext(ctx); state != nil {
		state.errored.Store(true)
		transactionName, endpoint := state.names()
		return traceFields{traceID: state.traceID, transactionName: transactionName, endpoint: endpoint}
	}
	return traceFields{traceID: TraceID(ctx)}
}

// MarkRequestErrored records that the request ctx belongs to failed without an error being
// reported through this client (a panic an integration saw pass through, for example), so its
// trace is still sent however fast it was. Errors reported with CaptureErrorCtx/RecoverCtx already
// do this themselves. A no-op outside a request.
func MarkRequestErrored(ctx context.Context) {
	if state := requestFromContext(ctx); state != nil {
		state.errored.Store(true)
	}
}

func requestErrored(ctx context.Context) bool {
	state := requestFromContext(ctx)
	return state != nil && state.errored.Load()
}

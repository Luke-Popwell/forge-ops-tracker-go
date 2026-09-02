// Package nethttp wraps a plain net/http.Handler (or anything with the same middleware signature
// -- chi, gorilla/mux, and most lightweight routers all compose with a plain
// func(http.Handler) http.Handler) so a panic that escapes it is reported before continuing to
// unwind exactly as it would without this client. Go's own net/http.Server already recovers a
// per-request panic itself (logging it and closing the connection, without crashing the process),
// so re-panicking here is safe and changes nothing about how the server's own handling behaves --
// the same "report, then don't change program behavior" rule every other client in this repo
// follows for its own framework integrations.
package nethttp

import (
	"net/http"

	forgeops "github.com/Luke-Popwell/forge-ops-tracker-go"
)

// Middleware wraps next so any panic that escapes a handler is reported with the request's
// method/URL as context, then re-panicked unchanged.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer forgeops.Recover(map[string]any{
			"url":    r.URL.String(),
			"method": r.Method,
		})
		next.ServeHTTP(w, r)
	})
}

// Package gin provides a Recovery-style middleware for the Gin web framework. Install it INSTEAD
// OF gin.Recovery(), not alongside it: whichever recovery middleware sits closer to the handler on
// the call stack is the one whose deferred recover() actually sees a panic first, so stacking both
// would leave one of them permanently unused. Gin has no separate hook to subscribe to the way
// Rails' Rack middleware or Spring's @ControllerAdvice do -- recovery *is* the framework's error
// handling here, so this middleware has to be it, not observe one from outside.
package gin

import (
	"net/http"

	"github.com/gin-gonic/gin"

	forgeops "github.com/Luke-Popwell/forge-ops-tracker-go"
)

// Recovery reports any panic that escapes a later handler in the chain, with the request's
// method/URL as context, then aborts the request with a 500 -- the same response gin.Recovery()
// itself would send, just with the panic also reported first.
func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if v := recover(); v != nil {
				forgeops.CaptureError(forgeops.PanicError(v), map[string]any{
					"url":    c.Request.URL.String(),
					"method": c.Request.Method,
				})
				c.AbortWithStatus(http.StatusInternalServerError)
			}
		}()
		c.Next()
	}
}

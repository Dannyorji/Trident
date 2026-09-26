package middleware

import (
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/Depo-dev/trident/services/api/internal/httputil"
	"github.com/Depo-dev/trident/services/api/internal/metrics"
)

// Recover returns middleware that catches a panic anywhere in the handler
// chain it wraps, logs the stack, increments trident_panics_recovered_total,
// and responds with the standard error envelope instead of dropping the
// connection uncontained (issue #610).
//
// Must sit outermost, ahead of every other middleware, so a panic in any of
// them — not just in a leaf handler — is caught before it can unwind past
// this point and kill the connection with no 500, no audit row, and no
// metric.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				requestID := httputil.RequestIDFromContext(r.Context())
				slog.Error("panic recovered",
					"request_id", requestID,
					"method", r.Method,
					"path", r.URL.Path,
					"panic", rec,
					"stack", string(debug.Stack()),
				)
				metrics.PanicsRecoveredTotal.Inc()
				httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

package middleware

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type responseWriter struct {
	http.ResponseWriter
	wroteHeader bool
	status      int
}

func (rw *responseWriter) Unwrap() http.ResponseWriter { return rw.ResponseWriter }

func (rw *responseWriter) WriteHeader(code int) {
	if rw.wroteHeader {
		return
	}
	rw.wroteHeader = true
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	if !rw.wroteHeader {
		rw.WriteHeader(http.StatusOK)
	}
	return rw.ResponseWriter.Write(b)
}

// Timeout returns middleware that cancels the request context after duration.
// Handlers must respect ctx.Done() for cancellation to take effect; the
// middleware detects deadline exceeded only after ServeHTTP returns.
// Paths matching any prefix in excluded are passed through without a timeout.
func Timeout(duration time.Duration, excluded []string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for _, prefix := range excluded {
				if strings.HasPrefix(r.URL.Path, prefix) {
					next.ServeHTTP(w, r)
					return
				}
			}

			ctx, cancel := context.WithTimeout(r.Context(), duration)
			defer cancel()

			rw := &responseWriter{ResponseWriter: w}
			next.ServeHTTP(rw, r.WithContext(ctx))

			if ctx.Err() == context.DeadlineExceeded && !rw.wroteHeader {
				writeJSON(rw, http.StatusRequestTimeout, map[string]any{
					"error": map[string]any{
						"code":    "TIMEOUT",
						"message": "request timed out",
					},
				})
			}
		})
	}
}

// NewTimeoutFromEnv reads REQUEST_TIMEOUT_MS and returns configured Timeout middleware.
// Default is 30000ms. Excluded paths are /ws and /v1/events/stream.
func NewTimeoutFromEnv() func(http.Handler) http.Handler {
	const defaultMS = 30000
	excluded := []string{"/ws", "/graphql", "/v1/events/stream"}

	ms := defaultMS
	if raw := os.Getenv("REQUEST_TIMEOUT_MS"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			ms = v
		}
	}

	return Timeout(time.Duration(ms)*time.Millisecond, excluded)
}

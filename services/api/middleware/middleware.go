package middleware

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/Depo-dev/trident/services/api/internal/httputil"
	"github.com/google/uuid"
)

const RequestIDHeader = "X-Request-ID"

// RequestID middleware accepts a valid incoming X-Request-Id (so a caller can
// correlate a request end-to-end) or generates a UUID when absent/invalid,
// attaches it to the request context, and echoes it on the response header.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if !httputil.ValidRequestID(id) {
			id = uuid.New().String()
		}
		w.Header().Set(RequestIDHeader, id)
		ctx := httputil.ContextWithRequestID(r.Context(), id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// LoggingResponseWriter wraps http.ResponseWriter to capture status code
type LoggingResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (w *LoggingResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *LoggingResponseWriter) WriteHeader(code int) {
	w.statusCode = code
	w.ResponseWriter.WriteHeader(code)
}

// StructuredLogging middleware logs one structured line per request with a
// guaranteed field set — service, request_id, method, route, status, and
// latency_ms, plus api_key_id once a request authenticates — so every log
// line is queryable the same way regardless of which handler produced it
// (issue #239).
//
// mux is used to resolve the *registered route pattern* (e.g.
// "GET /v1/events/{id}") via mux.Handler, the same technique NewMetrics uses
// for the identical cardinality reason: the raw URL path would blow up
// cardinality on every distinct event/contract id. mux may be nil (as in
// tests that exercise a bare handler chain), in which case route falls back
// to the raw path.
func StructuredLogging(mux *http.ServeMux) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			// Get request ID from context (set by the RequestID middleware).
			requestID := httputil.RequestIDFromContext(r.Context())

			// Install the mutable log-state holder before calling next so
			// NewDBAuth (running deeper in the chain) can attach api_key_id
			// once it authenticates — see requestLogState for why this can't
			// be a plain context.WithValue.
			ctx, logState := withLogState(r.Context())
			r = r.WithContext(ctx)

			route := r.URL.Path
			if mux != nil {
				if _, pattern := mux.Handler(r); pattern != "" {
					route = pattern
				}
			}

			// Wrap response writer to capture status code
			wrapped := &LoggingResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}

			next.ServeHTTP(wrapped, r)

			duration := time.Since(start)

			// "service" is not repeated here — it is a base attribute on the
			// default logger (see initLogger), so it already appears on this
			// line and every other one without needing to be listed per call.
			attrs := []slog.Attr{
				slog.String("request_id", requestID),
				slog.String("method", r.Method),
				slog.String("route", route),
				slog.Int("status", wrapped.statusCode),
				slog.Int64("latency_ms", duration.Milliseconds()),
			}
			// Absent for unauthenticated/rejected requests (e.g. a 401 or a
			// rate-limit 429 before NewDBAuth runs) — there is no key to name.
			if logState.apiKeyID != "" {
				attrs = append(attrs, slog.String("api_key_id", logState.apiKeyID))
			}

			slog.LogAttrs(r.Context(), slog.LevelInfo, "http_request", attrs...)
		})
	}
}

// Chain combines multiple middleware
func Chain(handler http.Handler, middlewares ...func(http.Handler) http.Handler) http.Handler {
	for i := len(middlewares) - 1; i >= 0; i-- {
		handler = middlewares[i](handler)
	}
	return handler
}

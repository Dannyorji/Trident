package middleware

import (
	"net/http"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Tracer creates an OpenTelemetry tracer for the middleware package.
func Tracer() trace.Tracer {
	return otel.Tracer("github.com/Depo-dev/trident/services/api/middleware")
}

// TracingMiddleware instruments HTTP requests with OpenTelemetry tracing,
// extracting trace context from W3C traceparent headers and creating spans
// for each request. The trace context is attached to the request context
// so downstream gRPC calls inherit the same trace ID.
func TracingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Extract trace context from incoming request (W3C standard).
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))

		// Create a span for this HTTP request.
		tracer := Tracer()
		ctx, span := tracer.Start(
			ctx,
			r.Method+" "+r.URL.Path,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("http.method", r.Method),
				attribute.String("http.url", r.URL.String()),
				attribute.String("http.target", r.URL.Path),
				attribute.String("http.scheme", r.URL.Scheme),
				attribute.String("http.host", r.Host),
				attribute.String("http.client_ip", clientIP(r)),
			),
		)
		defer span.End()

		// Wrap the response writer to capture the status code.
		wrapped := &statusCapturingWriter{ResponseWriter: w}

		// Continue to the next handler with the enriched context.
		next.ServeHTTP(wrapped, r.WithContext(ctx))

		// Record the HTTP status code.
		span.SetAttributes(attribute.Int("http.status_code", wrapped.statusCode))
	})
}

// statusCapturingWriter wraps http.ResponseWriter to capture the status code.
type statusCapturingWriter struct {
	http.ResponseWriter
	statusCode int
}

func (w *statusCapturingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// WriteHeader records the status code before writing it.
func (w *statusCapturingWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

// Write records a 200 status code if none has been set yet.
func (w *statusCapturingWriter) Write(b []byte) (int, error) {
	if w.statusCode == 0 {
		w.statusCode = 200
	}
	return w.ResponseWriter.Write(b)
}

// clientIP extracts the client IP from the request, checking X-Forwarded-For
// and X-Real-IP headers for proxied requests.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// X-Forwarded-For can contain multiple IPs; take the first.
		if idx := strings.Index(xff, ","); idx != -1 {
			return strings.TrimSpace(xff[:idx])
		}
		return strings.TrimSpace(xff)
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return strings.TrimSpace(xri)
	}
	return r.RemoteAddr
}

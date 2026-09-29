package middleware_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Depo-dev/trident/services/api/internal/httputil"
	"github.com/Depo-dev/trident/services/api/middleware"
)

const reqIDHeader = "X-Request-ID"

// TestRequestID_AcceptsIncomingHeader asserts a valid client-supplied request
// id is threaded into the context and echoed unchanged on the response.
func TestRequestID_AcceptsIncomingHeader(t *testing.T) {
	const incoming = "client-req-abc123"

	var seen string
	h := middleware.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = httputil.RequestIDFromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	req.Header.Set(reqIDHeader, incoming)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if seen != incoming {
		t.Fatalf("context request id = %q, want %q", seen, incoming)
	}
	if got := rec.Header().Get(reqIDHeader); got != incoming {
		t.Fatalf("echoed header = %q, want %q", got, incoming)
	}
}

// TestRequestID_GeneratesWhenAbsentOrInvalid asserts a missing or malformed
// incoming id is replaced by a generated one.
func TestRequestID_GeneratesWhenAbsentOrInvalid(t *testing.T) {
	cases := map[string]string{
		"absent":       "",
		"whitespace":   "has space",
		"control char": "bad\nid",
	}
	for name, incoming := range cases {
		t.Run(name, func(t *testing.T) {
			h := middleware.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
			req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
			if incoming != "" {
				req.Header.Set(reqIDHeader, incoming)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			got := rec.Header().Get(reqIDHeader)
			if got == "" || got == incoming {
				t.Fatalf("expected generated id, got %q (incoming %q)", got, incoming)
			}
		})
	}
}

// TestRequestID_EndToEnd_LogsAndErrorEnvelope asserts the acceptance criterion:
// header in -> present in structured logs, response header, and error envelope.
func TestRequestID_EndToEnd_LogsAndErrorEnvelope(t *testing.T) {
	const incoming = "trace-e2e-42"

	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, nil)))
	defer slog.SetDefault(prev)

	// Handler emits an error via the ctx-aware writer.
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, "boom")
	})
	h := middleware.Chain(final, middleware.RequestID, middleware.StructuredLogging(nil))

	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	req.Header.Set(reqIDHeader, incoming)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// Response header echoed.
	if got := rec.Header().Get(reqIDHeader); got != incoming {
		t.Fatalf("echoed header = %q, want %q", got, incoming)
	}

	// Error envelope carries request_id.
	var body httputil.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body.Error.RequestID != incoming {
		t.Fatalf("error.request_id = %q, want %q", body.Error.RequestID, incoming)
	}

	// Structured log line carries the same id.
	if !strings.Contains(logBuf.String(), incoming) {
		t.Fatalf("log output missing request id %q: %s", incoming, logBuf.String())
	}
}

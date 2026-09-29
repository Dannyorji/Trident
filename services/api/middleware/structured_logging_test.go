package middleware_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Depo-dev/trident/services/api/middleware"
)

// withCapturedLogs installs a JSON slog handler writing into buf as the
// process default for the duration of the test, restoring the previous
// default on cleanup.
func withCapturedLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// lastLogLine decodes the final JSON line in buf.
func lastLogLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var line map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &line); err != nil {
		t.Fatalf("decode log line %q: %v", lines[len(lines)-1], err)
	}
	return line
}

// TestStructuredLogging_UsesRegisteredRoutePatternNotRawPath asserts the
// route field is the registered ServeMux pattern (issue #239) — the same
// cardinality-safe technique NewMetrics already uses — not the raw path,
// which would blow up cardinality on every distinct event/contract id.
func TestStructuredLogging_UsesRegisteredRoutePatternNotRawPath(t *testing.T) {
	buf := withCapturedLogs(t)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/events/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	h := middleware.StructuredLogging(mux)(mux)
	req := httptest.NewRequest(http.MethodGet, "/v1/events/abc-123", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	line := lastLogLine(t, buf)
	if got := line["route"]; got != "GET /v1/events/{id}" {
		t.Fatalf("route = %v, want registered pattern %q", got, "GET /v1/events/{id}")
	}
	if strings.Contains(line["route"].(string), "abc-123") {
		t.Fatalf("route leaked the raw path parameter: %v", line["route"])
	}
}

// TestStructuredLogging_FallsBackToRawPathWithoutMux covers the nil-mux case
// used by tests exercising a bare handler chain.
func TestStructuredLogging_FallsBackToRawPathWithoutMux(t *testing.T) {
	buf := withCapturedLogs(t)

	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := middleware.StructuredLogging(nil)(final)

	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	line := lastLogLine(t, buf)
	if got := line["route"]; got != "/v1/events" {
		t.Fatalf("route = %v, want raw path fallback %q", got, "/v1/events")
	}
}

// TestStructuredLogging_CapturesAPIKeyIDSetDeeperInChain is the core
// regression for issue #239: NewDBAuth authenticates and attaches
// api_key_id several layers below StructuredLogging, on a request/context
// that is a *descendant* of the one StructuredLogging itself holds — a
// plain context.WithValue set there could never flow back up to the log
// line. SetLogAPIKeyID must still make it onto the line via the shared
// mutable holder.
func TestStructuredLogging_CapturesAPIKeyIDSetDeeperInChain(t *testing.T) {
	buf := withCapturedLogs(t)

	const keyID = "11111111-1111-1111-1111-111111111111"
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulates NewDBAuth: derives a new context/request and calls its
		// own next with that — never touching StructuredLogging's request.
		middleware.SetLogAPIKeyID(r.Context(), keyID)
		w.WriteHeader(http.StatusOK)
	})
	h := middleware.StructuredLogging(nil)(final)

	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	line := lastLogLine(t, buf)
	if got := line["api_key_id"]; got != keyID {
		t.Fatalf("api_key_id = %v, want %q", got, keyID)
	}
}

// TestStructuredLogging_OmitsAPIKeyIDWhenUnauthenticated covers the common
// case (a 401 before auth ever runs, or a route auth intentionally skips):
// there must be no api_key_id field at all, not an empty string, so a log
// query for "api_key_id exists" reliably means "this request authenticated".
func TestStructuredLogging_OmitsAPIKeyIDWhenUnauthenticated(t *testing.T) {
	buf := withCapturedLogs(t)

	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	h := middleware.StructuredLogging(nil)(final)

	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	line := lastLogLine(t, buf)
	if _, present := line["api_key_id"]; present {
		t.Fatalf("api_key_id should be absent for an unauthenticated request, got %v", line["api_key_id"])
	}
}

// TestStructuredLogging_NeverLogsTheRawAPIKey is the regression test the
// issue explicitly asks for: raw keys/secrets must never reach the logs.
// StructuredLogging never reads X-API-Key itself, but this guards against a
// future change accidentally logging the header or the request wholesale.
func TestStructuredLogging_NeverLogsTheRawAPIKey(t *testing.T) {
	buf := withCapturedLogs(t)

	const rawKey = "trident_super-secret-key-value-do-not-log"
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only the opaque, non-secret key id belongs in logs.
		middleware.SetLogAPIKeyID(r.Context(), "key-id-not-the-raw-key")
		w.WriteHeader(http.StatusOK)
	})
	h := middleware.StructuredLogging(nil)(final)

	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	req.Header.Set("X-API-Key", rawKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if strings.Contains(buf.String(), rawKey) {
		t.Fatalf("log output leaked the raw API key: %s", buf.String())
	}
}

// TestStructuredLogging_EmitsLatencyAsMilliseconds asserts latency_ms is a
// JSON number (milliseconds), not a formatted duration string like "1.2ms" —
// the schema the docs promise, and what a log query needs to threshold on.
func TestStructuredLogging_EmitsLatencyAsMilliseconds(t *testing.T) {
	buf := withCapturedLogs(t)

	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := middleware.StructuredLogging(nil)(final)

	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	line := lastLogLine(t, buf)
	if _, ok := line["latency_ms"].(float64); !ok {
		t.Fatalf("latency_ms = %v (%T), want a JSON number", line["latency_ms"], line["latency_ms"])
	}
}

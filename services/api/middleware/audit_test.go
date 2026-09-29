package middleware

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestExtractClientIP_XForwardedFor(t *testing.T) {
	tests := []struct {
		name   string
		xff    string
		realIP string
		remote string
		want   string
	}{
		{
			name: "single public IP",
			xff:  "203.0.113.1",
			want: "203.0.113.1",
		},
		{
			name: "chain with public IP first",
			xff:  "203.0.113.1, 10.0.0.1, 192.168.1.1",
			want: "203.0.113.1",
		},
		{
			name: "chain with private IPs only returns first",
			xff:  "10.0.0.1, 192.168.1.1",
			want: "10.0.0.1",
		},
		{
			name: "chain with public IP last",
			xff:  "10.0.0.1, 203.0.113.1",
			want: "203.0.113.1",
		},
		{
			name:   "fallback to X-Real-IP",
			realIP: "203.0.113.42",
			want:   "203.0.113.42",
		},
		{
			name:   "fallback to RemoteAddr",
			remote: "192.0.2.1:12345",
			want:   "192.0.2.1",
		},
		{
			name:   "remote addr without port",
			remote: "192.0.2.1",
			want:   "192.0.2.1",
		},
		{
			name: "IPv6 X-Forwarded-For",
			xff:  "2001:db8::1, fe80::1",
			want: "2001:db8::1",
		},
		{
			name: "loopback and private — returns first",
			xff:  "127.0.0.1, 10.0.0.1",
			want: "127.0.0.1",
		},
		{
			name: "empty with spaces",
			xff:  "  ,  203.0.113.1  ",
			want: "203.0.113.1",
		},
		{
			name:   "prefer X-Forwarded-For over X-Real-IP",
			xff:    "203.0.113.1",
			realIP: "198.51.100.1",
			want:   "203.0.113.1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.xff != "" {
				req.Header.Set("X-Forwarded-For", tt.xff)
			}
			if tt.realIP != "" {
				req.Header.Set("X-Real-IP", tt.realIP)
			}
			if tt.remote != "" {
				req.RemoteAddr = tt.remote
			}
			got := ExtractClientIP(req)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAuditWriterWriteNonBlocking(t *testing.T) {
	// Create writer with very small channel to test non-blocking behavior
	logger := newTestLogger()
	// pool is nil to test channel behavior without DB
	logger.Warn("audit log channel full, dropping entry", "endpoint", "/test")

	aw := &AuditWriter{
		ch:     make(chan AuditEntry, 1),
		pool:   nil,
		logger: logger,
	}

	// Fill the channel
	aw.ch <- AuditEntry{Endpoint: "/first"} //nolint:staticcheck

	// Start background goroutine to drain
	go func() {
		for range aw.ch {
			// Drain
		}
	}()

	// Write should not panic even though channel was full
	aw.Write(AuditEntry{Endpoint: "/dropped"})
	aw.Close()
}

func TestAuditWriterCloseFlushes(t *testing.T) {
	logger := newTestLogger()
	aw := &AuditWriter{
		ch:     make(chan AuditEntry, 10),
		pool:   nil,
		logger: logger,
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		id := uuid.New()
		aw.Write(AuditEntry{
			APIKeyID:   &id,
			Endpoint:   "/v1/events",
			Method:     "GET",
			IP:         "203.0.113.1",
			StatusCode: 200,
			DurationMs: 42,
			RequestID:  "test-req-1",
			Timestamp:  time.Now(),
		})
		aw.Close()
	}()

	<-done
	// Reaching here without deadlock means Close worked correctly
}

func TestIsPrivateIP(t *testing.T) {
	tests := []struct {
		ip      string
		private bool
	}{
		{"10.0.0.1", true},
		{"192.168.1.1", true},
		{"172.16.0.1", true},
		{"127.0.0.1", true},
		{"::1", true},
		{"fe80::1", true},
		{"203.0.113.1", false},
		{"8.8.8.8", false},
		{"2001:db8::1", false}, // documentation prefix
		{"invalid", false},
	}

	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			got := isPrivateIP(tt.ip)
			if got != tt.private {
				t.Errorf("isPrivateIP(%q) = %v, want %v", tt.ip, got, tt.private)
			}
		})
	}
}

func TestAuditContextFunctions(t *testing.T) {
	ctx := t.Context()

	// Nil by default
	if id := AuditAPIKeyIDFromContext(ctx); id != nil {
		t.Error("expected nil API key ID from empty context")
	}
	if n := AuditNetworkFromContext(ctx); n != "" {
		t.Error("expected empty network from empty context")
	}
	if s := AuditAuthSourceFromContext(ctx); s != "" {
		t.Error("expected empty auth source from empty context")
	}

	// Set and retrieve
	id := uuid.New()
	ctx = WithAuditAPIKeyID(ctx, &id)
	ctx = WithAuditNetwork(ctx, "testnet")
	ctx = WithAuditAuthSource(ctx, "legacy-env")

	if got := AuditAPIKeyIDFromContext(ctx); got == nil || *got != id {
		t.Errorf("API key ID mismatch: got %v, want %v", got, id)
	}
	if got := AuditNetworkFromContext(ctx); got != "testnet" {
		t.Errorf("network mismatch: got %q, want %q", got, "testnet")
	}
	if got := AuditAuthSourceFromContext(ctx); got != "legacy-env" {
		t.Errorf("auth source mismatch: got %q, want %q", got, "legacy-env")
	}
}

func TestAuditResponseWriter_CapturesStatus(t *testing.T) {
	rw := httptest.NewRecorder()
	aw := &auditResponseWriter{ResponseWriter: rw, statusCode: 200}

	aw.WriteHeader(404)

	if aw.statusCode != 404 {
		t.Errorf("want status 404, got %d", aw.statusCode)
	}
	if rw.Code != 404 {
		t.Errorf("underlying recorder: want 404, got %d", rw.Code)
	}
}

// TestAuditMiddleware_WrapsAuth_LogsRejectedAuthAttempts is the regression
// test for issue #609: main.go previously built the handler chain as
// handler = AuditMiddleware(handler); handler = NewDBAuth(handler), which
// makes NewDBAuth the OUTER layer around AuditMiddleware. NewDBAuth's 401
// early-return never calls next.ServeHTTP, so the inner AuditMiddleware
// never ran and a failed authentication attempt left no audit_log row at
// all — exactly the traffic a security audit trail most needs to capture.
// This builds the chain in the correct order (AuditMiddleware outermost,
// wrapping NewDBAuth) and asserts a rejected request still produces an
// audit entry with the 401 status recorded.
func TestAuditMiddleware_WrapsAuth_LogsRejectedAuthAttempts(t *testing.T) {
	entries := make(chan AuditEntry, 10)
	aw := &AuditWriter{
		ch:     entries,
		pool:   nil,
		logger: newTestLogger(),
	}

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler must not be reached: request has no X-API-Key and no auth is configured")
	})

	// Mirrors main.go's real composition order: AuditMiddleware wraps
	// NewDBAuth, so it observes every request regardless of auth outcome.
	handler := AuditMiddleware(aw)(NewDBAuth(DBAuthConfig{})(inner))

	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	req.RemoteAddr = "192.0.2.1:12345"
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}

	select {
	case entry := <-entries:
		if entry.StatusCode != http.StatusUnauthorized {
			t.Errorf("audit entry status = %d, want 401", entry.StatusCode)
		}
		if entry.Endpoint != "/v1/events" {
			t.Errorf("audit entry endpoint = %q, want /v1/events", entry.Endpoint)
		}
		if entry.FailureReason == "" {
			t.Error("audit entry has no failure_reason for a rejected auth attempt")
		}
		if entry.AttemptedKeyPrefix != "" {
			t.Errorf("attempted_key_prefix = %q, want empty (no X-API-Key header was sent)", entry.AttemptedKeyPrefix)
		}
	case <-time.After(time.Second):
		t.Fatal("rejected auth attempt produced no audit entry at all")
	}
}

// TestAuditMiddleware_RecordsAttemptedKeyPrefixNeverFullKey is the
// regression test for #609's "record the attempted key prefix — never the
// full key — and the failure reason" scope item: a wrong-but-present key
// must audit only a bounded prefix of it, never the value in full.
func TestAuditMiddleware_RecordsAttemptedKeyPrefixNeverFullKey(t *testing.T) {
	entries := make(chan AuditEntry, 10)
	aw := &AuditWriter{
		ch:     entries,
		pool:   nil,
		logger: newTestLogger(),
	}

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler must not be reached: the key does not resolve to any api_keys row")
	})

	handler := AuditMiddleware(aw)(NewDBAuth(DBAuthConfig{})(inner))

	wrongKey := "trident_deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	req.Header.Set("X-API-Key", wrongKey)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}

	select {
	case entry := <-entries:
		if entry.AttemptedKeyPrefix == "" {
			t.Fatal("attempted_key_prefix is empty for a present-but-invalid key")
		}
		if entry.AttemptedKeyPrefix == wrongKey {
			t.Fatal("attempted_key_prefix stored the full attempted key, not a prefix")
		}
		if len(entry.AttemptedKeyPrefix) != auditKeyPrefixLength {
			t.Errorf("attempted_key_prefix length = %d, want %d", len(entry.AttemptedKeyPrefix), auditKeyPrefixLength)
		}
		if entry.AttemptedKeyPrefix != wrongKey[:auditKeyPrefixLength] {
			t.Errorf("attempted_key_prefix = %q, want %q", entry.AttemptedKeyPrefix, wrongKey[:auditKeyPrefixLength])
		}
		if entry.FailureReason == "" {
			t.Error("audit entry has no failure_reason")
		}
	case <-time.After(time.Second):
		t.Fatal("rejected auth attempt produced no audit entry at all")
	}
}

func TestAuditMiddleware_CreatesEntry(t *testing.T) {
	entries := make(chan AuditEntry, 10)
	aw := &AuditWriter{
		ch:     entries,
		pool:   nil,
		logger: newTestLogger(),
	}

	handler := AuditMiddleware(aw)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	req.RemoteAddr = "192.0.2.1:12345"
	req.Header.Set("User-Agent", "test-agent")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	select {
	case entry := <-entries:
		if entry.Endpoint != "/v1/events" {
			t.Errorf("want endpoint /v1/events, got %q", entry.Endpoint)
		}
		if entry.Method != "GET" {
			t.Errorf("want method GET, got %q", entry.Method)
		}
		if entry.StatusCode != 200 {
			t.Errorf("want status 200, got %d", entry.StatusCode)
		}
		if entry.UserAgent != "test-agent" {
			t.Errorf("want user-agent test-agent, got %q", entry.UserAgent)
		}
		if entry.IP != "192.0.2.1" {
			t.Errorf("want IP 192.0.2.1, got %q", entry.IP)
		}
		if entry.APIKeyID != nil {
			t.Errorf("expected nil API key ID for unauthenticated request")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for audit entry")
	}
}

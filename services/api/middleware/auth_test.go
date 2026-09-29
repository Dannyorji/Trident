package middleware_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Depo-dev/trident/services/api/internal/httputil"
	"github.com/Depo-dev/trident/services/api/middleware"
)

func hashKey(salt, key string) string {
	mac := hmac.New(sha256.New, []byte(salt))
	_, _ = mac.Write([]byte(key))
	return hex.EncodeToString(mac.Sum(nil))
}

func TestAPIKey(t *testing.T) {
	const (
		salt = "test-salt"
		key  = "valid-key-32-byte-hex-string-format"
	)
	t.Setenv("API_KEY_SALT", salt)
	t.Setenv("API_KEY_HASHES", hashKey(salt, key))

	handler := middleware.APIKey(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	tests := []struct {
		name       string
		path       string
		key        string
		wantStatus int
		checkBody  bool
	}{
		{name: "valid protected request", path: "/v1/events/stream", key: key, wantStatus: http.StatusNoContent},
		{name: "missing key", path: "/v1/events/stream", wantStatus: http.StatusUnauthorized, checkBody: true},
		{name: "invalid key", path: "/v1/events/stream", key: "wrong-key", wantStatus: http.StatusUnauthorized, checkBody: true},
		{name: "health is public", path: "/v1/health", wantStatus: http.StatusNoContent},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			if tt.key != "" {
				req.Header.Set("X-API-Key", tt.key)
			}
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status: got %d, want %d", rec.Code, tt.wantStatus)
			}

			if tt.checkBody {
				var errResp httputil.ErrorResponse
				if err := json.NewDecoder(rec.Body).Decode(&errResp); err != nil {
					t.Fatalf("failed to decode error body: %v", err)
				}
				if errResp.Error.Code != httputil.UNAUTHORIZED {
					t.Errorf("error code: got %q, want %q", errResp.Error.Code, httputil.UNAUTHORIZED)
				}
			}
		})
	}
}

func TestSingleAPIKeyEnv(t *testing.T) {
	const (
		salt = "test-salt"
		key  = "single-env-key-value"
	)
	t.Setenv("API_KEY_SALT", salt)
	t.Setenv("API_KEY_HASHES", "")
	t.Setenv("API_KEY", hashKey(salt, key))

	handler := middleware.APIKey(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	req.Header.Set("X-API-Key", key)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestConstantTimeContains(t *testing.T) {
	hashes := map[string]struct{}{
		"a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90": {},
		"11223344556677889900aabbccddeeff11223344556677889900aabbccddeeff": {},
	}

	tests := []struct {
		name     string
		target   string
		expected bool
	}{
		{
			name:     "exact match first",
			target:   "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90",
			expected: true,
		},
		{
			name:     "exact match second",
			target:   "11223344556677889900aabbccddeeff11223344556677889900aabbccddeeff",
			expected: true,
		},
		{
			name:     "same length non match",
			target:   "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f99",
			expected: false,
		},
		{
			name:     "different length",
			target:   "a1b2c3d4e5f60718293a4b5c6d7e8f90",
			expected: false,
		},
		{
			name:     "empty string",
			target:   "",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := middleware.ConstantTimeContains(hashes, tt.target)
			if got != tt.expected {
				t.Errorf("ConstantTimeContains(%q) = %v, want %v", tt.target, got, tt.expected)
			}
		})
	}
}

func TestNoRawKeyLeakage(t *testing.T) {
	// Verify that ParseKeyHashes stores hashes and not raw values
	raw := "hash1,hash2"
	hashes := middleware.ParseKeyHashes(raw)

	if _, ok := hashes["hash1"]; !ok {
		t.Error("expected hash1 in parsed key hashes")
	}
	if _, ok := hashes["hash2"]; !ok {
		t.Error("expected hash2 in parsed key hashes")
	}
	for k := range hashes {
		if strings.Contains(k, "raw-key-value") {
			t.Errorf("found raw key in hashes: %s", k)
		}
	}
}


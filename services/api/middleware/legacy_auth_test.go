package middleware

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestWithLegacyAuthenticatedKey_AttachesIdentityNetworkAndAuditSource is
// the direct unit test for issue #616's fix: a request authenticated via
// the legacy API_KEY_HASHES env-var path must carry a non-empty key id
// (even if synthetic), an explicit network, and an audit attribution -
// none of which the pre-fix code attached at all.
func TestWithLegacyAuthenticatedKey_AttachesIdentityNetworkAndAuditSource(t *testing.T) {
	ctx := withLegacyAuthenticatedKey(t.Context())

	if got := APIKeyIDFromContext(ctx); got != LegacyEnvKeyID {
		t.Errorf("APIKeyIDFromContext: got %q, want sentinel %q", got, LegacyEnvKeyID)
	}
	if got := NetworkFromContext(ctx); got != LegacyEnvNetwork {
		t.Errorf("NetworkFromContext: got %q, want %q", got, LegacyEnvNetwork)
	}
	if got := AuditNetworkFromContext(ctx); got != LegacyEnvNetwork {
		t.Errorf("AuditNetworkFromContext: got %q, want %q", got, LegacyEnvNetwork)
	}
	if got := AuditAuthSourceFromContext(ctx); got != "legacy-env" {
		t.Errorf("AuditAuthSourceFromContext: got %q, want %q", got, "legacy-env")
	}
	// AuditAPIKeyIDFromContext must stay nil: audit_log.api_key_id has a
	// foreign key to api_keys, and a legacy env-var key has no row there.
	// Fabricating a UUID here would either violate that constraint or
	// misattribute the request to an unrelated real key.
	if got := AuditAPIKeyIDFromContext(ctx); got != nil {
		t.Errorf("AuditAPIKeyIDFromContext: got %v, want nil (legacy keys have no api_keys row)", got)
	}
}

// TestNewDBAuth_LegacyPath_AttachesIdentity is the end-to-end regression
// test for issue #616: before the fix, a request that authenticated via
// the legacy env-var fallback reached the handler with
// APIKeyIDFromContext == "" (indistinguishable from auth never having run)
// and no audit attribution at all. This drives a full NewDBAuth chain (no
// DB, no Redis, so only the legacy branch can succeed) and asserts the
// handler sees the sentinel identity and explicit network.
func TestNewDBAuth_LegacyPath_AttachesIdentity(t *testing.T) {
	const (
		salt = "legacy-test-salt"
		key  = "legacy-env-test-key"
	)
	t.Setenv("API_KEY_SALT", salt)
	mac := hmac.New(sha256.New, []byte(salt))
	mac.Write([]byte(key))
	t.Setenv("API_KEY_HASHES", hex.EncodeToString(mac.Sum(nil)))

	var gotKeyID, gotNetwork string
	handler := NewDBAuth(DBAuthConfig{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKeyID = APIKeyIDFromContext(r.Context())
		gotNetwork = NetworkFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	req.Header.Set("X-API-Key", key)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
	if gotKeyID != LegacyEnvKeyID {
		t.Errorf("handler saw APIKeyIDFromContext %q, want sentinel %q", gotKeyID, LegacyEnvKeyID)
	}
	if gotNetwork != LegacyEnvNetwork {
		t.Errorf("handler saw NetworkFromContext %q, want %q", gotNetwork, LegacyEnvNetwork)
	}
}

// TestNewDBAuth_LegacyPath_WrongKeyStillRejected guards against the fix
// accidentally loosening the legacy path's own authentication check: a key
// that doesn't match any configured hash must still be rejected with 401,
// exactly as before this change.
func TestNewDBAuth_LegacyPath_WrongKeyStillRejected(t *testing.T) {
	const salt = "legacy-test-salt-2"
	t.Setenv("API_KEY_SALT", salt)
	mac := hmac.New(sha256.New, []byte(salt))
	mac.Write([]byte("the-real-key"))
	t.Setenv("API_KEY_HASHES", hex.EncodeToString(mac.Sum(nil)))

	handler := NewDBAuth(DBAuthConfig{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	req.Header.Set("X-API-Key", "not-the-real-key")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

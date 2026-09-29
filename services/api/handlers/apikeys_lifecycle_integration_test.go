package handlers_test

// Issue #516: API key lifecycle — issue, rotate, revoke, and audit.
//
// The "Done when" bar for #516 is that a key can be issued, rotated, and
// revoked end to end, with revocation proven to take effect immediately —
// including on the cached validation path (docs/runbooks/api-key-lifecycle.md
// already documents this contract in detail). The handlers
// (CreateAPIKey/ListAPIKeys/UpdateAPIKey/DeleteAPIKey in apikeys.go) and the
// Redis-cache-aware auth middleware (NewDBAuth in middleware/auth.go) already
// implement this correctly, but nothing exercised it end to end: there was no
// test file for apikeys.go at all, and auth_test.go only covers the legacy
// API_KEY_HASHES env-var path, a completely different code path from the
// DB-backed api_keys table these handlers manage.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Depo-dev/trident/services/api/handlers"
	"github.com/Depo-dev/trident/services/api/middleware"
	"github.com/redis/go-redis/v9"
)

const testLifecycleAdminKey = "test-admin-key-for-lifecycle-integration"

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// connectRealTestRedis mirrors connectRealTestDB's skip/hard-fail convention
// (see usage_rollup_integration_test.go) for TEST_REDIS_URL, and
// stream_integration_test.go's redis.ParseURL(TEST_REDIS_URL) convention for
// the value's format (a full "redis://host:port" URL, not a bare address).
func connectRealTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	redisURL, ok := os.LookupEnv("TEST_REDIS_URL")
	if !ok {
		if os.Getenv("REQUIRE_TEST_SERVICES") != "" {
			t.Fatal("TEST_REDIS_URL must be set when REQUIRE_TEST_SERVICES is set")
		}
		t.Skip("SKIP: TEST_REDIS_URL not set")
	}

	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Fatalf("parse TEST_REDIS_URL: %v", err)
	}
	client := redis.NewClient(opts)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("connect TEST_REDIS_URL: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func createKeyViaHandler(t *testing.T, cfg handlers.APIKeyConfig, body string) handlers.APIKeyResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/api-keys", strings.NewReader(body))
	req.Header.Set("X-Admin-Key", testLifecycleAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handlers.CreateAPIKey(cfg).ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create key: status %d, body %s", rec.Code, rec.Body.String())
	}

	var resp handlers.APIKeyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if resp.Key == nil {
		t.Fatal("create response missing plaintext key")
	}
	return resp
}

func authenticateWith(cfg middleware.DBAuthConfig, key string) int {
	handler := middleware.NewDBAuth(cfg)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	req.Header.Set("X-API-Key", key)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Code
}

// TestAPIKeyLifecycle_IssueAuthenticateRevokeIsImmediate is the acceptance
// test for #516: issues a key, uses it to authenticate (which populates the
// Redis cache), revokes it, then authenticates again with the SAME key and
// requires an immediate rejection — not "eventually, after the cache TTL".
func TestAPIKeyLifecycle_IssueAuthenticateRevokeIsImmediate(t *testing.T) {
	pool := connectRealTestDB(t)
	rdb := connectRealTestRedis(t)
	ctx := context.Background()

	cfg := handlers.APIKeyConfig{AdminKey: testLifecycleAdminKey, DB: pool, Redis: rdb}
	authCfg := middleware.DBAuthConfig{DB: pool, Redis: rdb}

	created := createKeyViaHandler(t, cfg,
		`{"label":"lifecycle-test","network":"testnet","rate_limit_tier":"standard","created_by":"integration-test"}`)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM api_keys WHERE id = $1`, created.ID)
	})

	// 1. A freshly issued key authenticates successfully.
	if code := authenticateWith(authCfg, *created.Key); code != http.StatusNoContent {
		t.Fatalf("fresh key: got status %d, want %d", code, http.StatusNoContent)
	}

	// 2. That first successful auth populated the Redis cache — confirm it's
	// actually there, so the revocation check below is genuinely proving
	// cache invalidation and not just an empty cache that would pass anyway.
	dbHash := sha256Hex(*created.Key)
	cachedVal, err := rdb.Get(ctx, "apiauth:"+dbHash).Result()
	if err != nil {
		t.Fatalf("expected auth cache entry to exist after a successful auth, got error: %v", err)
	}
	if cachedVal == "" {
		t.Fatal("expected a non-empty cached auth entry")
	}

	// 3. Revoke the key via the same DeleteAPIKey handler an admin would use.
	req := httptest.NewRequest(http.MethodDelete, "/v1/api-keys/"+created.ID, nil)
	req.SetPathValue("id", created.ID)
	req.Header.Set("X-Admin-Key", testLifecycleAdminKey)
	rec := httptest.NewRecorder()
	handlers.DeleteAPIKey(cfg).ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: status %d, body %s", rec.Code, rec.Body.String())
	}

	// 4. The Redis cache entry must be gone immediately — this is the
	// concrete mechanism that makes revocation immediate rather than
	// TTL-bound (authCacheTTL is 5 minutes; this test must not need to wait
	// anywhere near that long).
	if _, err := rdb.Get(ctx, "apiauth:"+dbHash).Result(); err != redis.Nil {
		t.Fatalf("expected auth cache entry to be evicted immediately on revocation, got err=%v", err)
	}

	// 5. Authenticating with the SAME plaintext key immediately after
	// revocation must fail — this is the actual end-to-end proof the issue
	// asks for: no window where a revoked key still works because of a
	// stale cache entry.
	if code := authenticateWith(authCfg, *created.Key); code != http.StatusUnauthorized {
		t.Fatalf("revoked key: got status %d, want %d (revocation was not immediate)", code, http.StatusUnauthorized)
	}

	// 6. Revoking an already-revoked key is a clean 404, not a silent
	// success or a 500 — DeleteAPIKey's UPDATE ... WHERE revoked_at IS NULL
	// only matches an active key.
	rec2 := httptest.NewRecorder()
	handlers.DeleteAPIKey(cfg).ServeHTTP(rec2, req)
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("double revoke: got status %d, want %d", rec2.Code, http.StatusNotFound)
	}
}

// TestAPIKeyLifecycle_RotationOverlapWindow proves the documented rotation
// procedure: creating a new key does not touch the old one, so both remain
// valid simultaneously during an overlap window, and only revoking the old
// key at the end of that window cuts it off — while the new key is
// unaffected throughout.
func TestAPIKeyLifecycle_RotationOverlapWindow(t *testing.T) {
	pool := connectRealTestDB(t)
	rdb := connectRealTestRedis(t)

	cfg := handlers.APIKeyConfig{AdminKey: testLifecycleAdminKey, DB: pool, Redis: rdb}
	authCfg := middleware.DBAuthConfig{DB: pool, Redis: rdb}

	oldKey := createKeyViaHandler(t, cfg,
		`{"label":"rotation-old","network":"testnet","rate_limit_tier":"standard"}`)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM api_keys WHERE id = $1`, oldKey.ID)
	})

	// Rotate: issue the new key BEFORE revoking the old one.
	newKey := createKeyViaHandler(t, cfg,
		`{"label":"rotation-new","network":"testnet","rate_limit_tier":"standard"}`)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM api_keys WHERE id = $1`, newKey.ID)
	})

	// Overlap window: BOTH keys authenticate successfully.
	if code := authenticateWith(authCfg, *oldKey.Key); code != http.StatusNoContent {
		t.Fatalf("old key during overlap: got status %d, want %d", code, http.StatusNoContent)
	}
	if code := authenticateWith(authCfg, *newKey.Key); code != http.StatusNoContent {
		t.Fatalf("new key during overlap: got status %d, want %d", code, http.StatusNoContent)
	}

	// End the overlap window: revoke only the old key.
	req := httptest.NewRequest(http.MethodDelete, "/v1/api-keys/"+oldKey.ID, nil)
	req.SetPathValue("id", oldKey.ID)
	req.Header.Set("X-Admin-Key", testLifecycleAdminKey)
	rec := httptest.NewRecorder()
	handlers.DeleteAPIKey(cfg).ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revoke old key: status %d", rec.Code)
	}

	// Old key is now rejected...
	if code := authenticateWith(authCfg, *oldKey.Key); code != http.StatusUnauthorized {
		t.Fatalf("old key after cutover: got status %d, want %d", code, http.StatusUnauthorized)
	}
	// ...but the new key is completely unaffected by the old key's revocation.
	if code := authenticateWith(authCfg, *newKey.Key); code != http.StatusNoContent {
		t.Fatalf("new key after old-key revocation: got status %d, want %d (rotation leaked into an unrelated key)", code, http.StatusNoContent)
	}
}

// TestAPIKeyLifecycle_AuditTrailRecordsUsage proves the "key usage is
// auditable: who used which key, when" criterion. This writes the
// audit_log row directly (matching how the real audit middleware attributes
// a request via WithAuditAPIKeyID(ctx)) to keep this test focused on the
// query surface an operator actually runs during an incident — see
// api-key-lifecycle.md's "Suspected or confirmed compromise" section —
// rather than re-testing the audit middleware itself, which has its own
// coverage in middleware/audit_test.go.
func TestAPIKeyLifecycle_AuditTrailRecordsUsage(t *testing.T) {
	pool := connectRealTestDB(t)
	ctx := context.Background()

	cfg := handlers.APIKeyConfig{AdminKey: testLifecycleAdminKey, DB: pool}
	created := createKeyViaHandler(t, cfg, `{"label":"audit-test","network":"testnet"}`)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM audit_log WHERE api_key_id = $1`, created.ID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM api_keys WHERE id = $1`, created.ID)
	})

	_, err := pool.Exec(ctx,
		`INSERT INTO audit_log (api_key_id, endpoint, method, status_code, duration_ms, request_id, ip, ts)
		 VALUES ($1, '/v1/events', 'GET', 200, 12, 'lifecycle-audit-req-1', '203.0.113.5', NOW())`,
		created.ID,
	)
	if err != nil {
		t.Fatalf("insert audit_log row: %v", err)
	}

	var count int
	var ip string
	err = pool.QueryRow(ctx,
		`SELECT COUNT(*), MAX(ip)::text FROM audit_log WHERE api_key_id = $1`, created.ID,
	).Scan(&count, &ip)
	if err != nil {
		t.Fatalf("query audit_log: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 audit_log row for this key, got %d", count)
	}
	// Postgres renders a single-address INET as text with a /32 (IPv4) or
	// /128 (IPv6) suffix — expected, not a bug in the audit write path.
	if ip != "203.0.113.5/32" {
		t.Fatalf("expected audit_log to record the requesting IP, got %q", ip)
	}
}

package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// recordingAPIKeysDB wraps fakeAPIKeysDB (declared in graphql_auth_test.go)
// and records every query it is asked to run, so a test can assert on what
// SQL NewDBAuth actually issues — specifically, that an unresolvable key
// never reaches an INSERT.
//
// This is the regression test for issue #606: resolveAPIKeyID in
// services/api/webhooks.go used to compare the raw X-API-Key header
// directly against api_keys.id (a primary key, not a secret hash) and, on
// any miss, fell through to `INSERT INTO api_keys DEFAULT VALUES`, minting
// an unauthenticated, unlabeled, never-revoked key row for every bad
// request. The real fix lives one layer up, in NewDBAuth itself (the
// middleware every /v1 route runs through before a handler like webhooks.go
// ever sees the request): it resolves a key by hashing it with
// sha256KeyHash and querying `WHERE key_hash = $1 AND revoked_at IS NULL`,
// and on a miss falls through to the legacy env-hash path and then a plain
// 401 — there is no INSERT anywhere in this function. This test proves that
// behavior directly against NewDBAuth rather than trusting a code review.
type recordingAPIKeysDB struct {
	fakeAPIKeysDB
	queries []string
}

func (d *recordingAPIKeysDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	d.queries = append(d.queries, sql)
	return d.fakeAPIKeysDB.QueryRow(ctx, sql, args...)
}

func newAuthTestRequest(path, key string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	return req
}

// TestNewDBAuth_UnknownKeyRejectedAndCreatesNoRow is issue #606's first and
// third "done when" criteria: an unknown X-API-Key returns 401, and the
// database is never asked to create anything on the way there.
func TestNewDBAuth_UnknownKeyRejectedAndCreatesNoRow(t *testing.T) {
	db := &recordingAPIKeysDB{fakeAPIKeysDB: fakeAPIKeysDB{found: false}}
	handler := NewDBAuth(DBAuthConfig{DB: db})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, newAuthTestRequest("/v1/webhooks", "bogus-unknown-key"))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want %d (unknown key must be rejected, not passed through)", rec.Code, http.StatusUnauthorized)
	}

	for _, q := range db.queries {
		if containsInsert(q) {
			t.Fatalf("NewDBAuth issued an INSERT for an unresolvable key: %q — this is exactly the api_keys-minting bug issue #606 fixed", q)
		}
	}
}

// TestNewDBAuth_ValidKeyResolvesToOwnIDOnly is issue #606's second "done
// when": a valid key resolves to exactly its own id, and never mints or
// substitutes a different one.
func TestNewDBAuth_ValidKeyResolvesToOwnIDOnly(t *testing.T) {
	const wantID = "33333333-3333-3333-3333-333333333333"
	db := &recordingAPIKeysDB{fakeAPIKeysDB: fakeAPIKeysDB{found: true, id: wantID, network: "mainnet"}}

	var gotID string
	handler := NewDBAuth(DBAuthConfig{DB: db})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotID = APIKeyIDFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, newAuthTestRequest("/v1/webhooks", "a-real-key"))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusNoContent)
	}
	if gotID != wantID {
		t.Errorf("resolved api_key_id: got %q, want %q (its own id, and no other)", gotID, wantID)
	}

	for _, q := range db.queries {
		if containsInsert(q) {
			t.Fatalf("NewDBAuth issued an INSERT while resolving a valid key: %q", q)
		}
	}
}

// TestNewDBAuth_NoDBConfiguredCreatesNoRowAndRejects covers the case
// resolveAPIKeyID's old fallback actually ran in: no DB-backed match at all
// (here modeled by DB: nil, i.e. the lookup is skipped entirely) and no
// legacy API_KEY_HASHES configured either. The only correct outcome is 401;
// there is no code path left that can fall through to an INSERT.
func TestNewDBAuth_NoDBConfiguredCreatesNoRowAndRejects(t *testing.T) {
	t.Setenv("API_KEY_HASHES", "")
	t.Setenv("API_KEY", "")

	handler := NewDBAuth(DBAuthConfig{})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, newAuthTestRequest("/v1/webhooks", "anything"))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func containsInsert(sql string) bool {
	return strings.Contains(strings.ToUpper(sql), "INSERT")
}

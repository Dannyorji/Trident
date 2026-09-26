package middleware

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
)

// newGraphQLAuthTestRedis is package-local (this file is package middleware,
// not middleware_test, so it can use the unexported sha256KeyHash/
// authRedisCacheKey/authCacheTTL helpers GraphQLDBAuth itself uses) —
// mirrors cache_test.go's newCacheTestRedis, which lives in the external
// test package and so isn't reachable from here.
func newGraphQLAuthTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// fakeAPIKeysDB is a minimal handlers.DB-shaped stand-in for the api_keys
// lookup GraphQLDBAuth (and NewDBAuth) run: `SELECT id, network FROM
// api_keys WHERE key_hash = $1 AND revoked_at IS NULL`. found=false models a
// revoked or unknown key exactly as that WHERE clause would: no matching
// row, so QueryRow's Scan returns pgx.ErrNoRows.
type fakeAPIKeysDB struct {
	found   bool
	id      string
	network string
}

func (d *fakeAPIKeysDB) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	return &fakeAPIKeysRow{found: d.found, id: d.id, network: d.network}
}

type fakeAPIKeysRow struct {
	found   bool
	id      string
	network string
}

func (r *fakeAPIKeysRow) Scan(dest ...any) error {
	if !r.found {
		return pgx.ErrNoRows
	}
	*dest[0].(*string) = r.id
	*dest[1].(*string) = r.network
	return nil
}

// TestGraphQLDBAuth_DBIssuedKeyAuthenticates covers issue #614's first "done
// when": a DB-issued key authenticates against /graphql. Before the fix,
// /graphql's Auth was wired to middleware.Validator, which does a map lookup
// against API_KEY_HASHES with no database path at all — a DB-issued key
// could never match it. GraphQLDBAuth is the real path actually wired at
// routes.go (Auth: middleware.GraphQLDBAuth(d.authDB)); this asserts it
// resolves a DB-backed key on a cache miss.
func TestGraphQLDBAuth_DBIssuedKeyAuthenticates(t *testing.T) {
	db := &fakeAPIKeysDB{found: true, id: "11111111-1111-1111-1111-111111111111", network: "mainnet"}
	authFn := GraphQLDBAuth(DBAuthConfig{DB: db})

	id, network, ok := authFn(context.Background(), "some-db-issued-key")
	if !ok {
		t.Fatal("expected a DB-issued key to authenticate")
	}
	if id != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("keyID: got %q, want the api_keys row id", id)
	}
	if network != "mainnet" {
		t.Errorf("network: got %q, want %q", network, "mainnet")
	}
}

// TestGraphQLDBAuth_RevokedKeyRejected covers issue #614's second "done
// when": a revoked key is rejected. revoked_at IS NULL in the query means a
// revoked row simply never matches, which fakeAPIKeysDB models as found=false
// (no cache entry either, so this exercises the DB path directly).
func TestGraphQLDBAuth_RevokedKeyRejected(t *testing.T) {
	db := &fakeAPIKeysDB{found: false}
	authFn := GraphQLDBAuth(DBAuthConfig{DB: db})

	_, _, ok := authFn(context.Background(), "revoked-key")
	if ok {
		t.Fatal("expected a revoked/unknown key to be rejected")
	}
}

// TestGraphQLDBAuth_RevocationTakesEffectOnNextConnection proves the second
// half of "rejected on the next connection attempt": once a key is revoked
// in the database, GraphQLDBAuth must not keep honoring a stale Redis cache
// entry beyond its TTL — a fresh connection attempt after the cache entry is
// gone (simulated here by a cache that never has the entry, i.e. a
// connection made after the 5-minute TTL elapsed) re-checks the database and
// is rejected.
func TestGraphQLDBAuth_RevocationTakesEffectOnNextConnection(t *testing.T) {
	rdb := newGraphQLAuthTestRedis(t)
	db := &fakeAPIKeysDB{found: false} // revoked: no matching row anymore
	authFn := GraphQLDBAuth(DBAuthConfig{DB: db, Redis: rdb})

	_, _, ok := authFn(context.Background(), "just-revoked-key")
	if ok {
		t.Fatal("expected a next-connection-attempt lookup for a revoked key to be rejected")
	}
}

// TestGraphQLDBAuth_CachedKeyAuthenticatesWithoutDBHit proves the Redis
// cache path GraphQLDBAuth shares with NewDBAuth: a key already cached in the
// "<uuid>:<network>" format authenticates without a DB lookup at all.
func TestGraphQLDBAuth_CachedKeyAuthenticatesWithoutDBHit(t *testing.T) {
	rdb := newGraphQLAuthTestRedis(t)
	const key = "cached-key"
	rdb.Set(context.Background(), authRedisCacheKey(sha256KeyHash(key)), "22222222-2222-2222-2222-222222222222:testnet", authCacheTTL)

	// No DB configured at all: if this authenticates, it can only be via
	// the cache.
	authFn := GraphQLDBAuth(DBAuthConfig{Redis: rdb})

	id, network, ok := authFn(context.Background(), key)
	if !ok {
		t.Fatal("expected a cached key to authenticate")
	}
	if id != "22222222-2222-2222-2222-222222222222" || network != "testnet" {
		t.Errorf("got id=%q network=%q, want the cached values", id, network)
	}
}

// TestGraphQLDBAuth_NoAuthConfigured matches the "auth not configured"
// posture documented on GraphQLDBAuth: with neither DB nor Redis, every key
// is accepted on the default network, mirroring how Auth() behaves with an
// empty hash set for local development.
func TestGraphQLDBAuth_NoAuthConfigured(t *testing.T) {
	authFn := GraphQLDBAuth(DBAuthConfig{})

	_, network, ok := authFn(context.Background(), "anything")
	if !ok {
		t.Fatal("expected auth to be a no-op when neither DB nor Redis is configured")
	}
	if network != "testnet" {
		t.Errorf("network: got %q, want default %q", network, "testnet")
	}
}

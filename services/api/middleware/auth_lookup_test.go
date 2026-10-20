package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Depo-dev/trident/services/api/internal/metrics"
	"github.com/alicebob/miniredis/v2"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
)

// scriptedKeyDB answers the api_keys lookup with a fixed id/network/expiry or
// a fixed error.
type scriptedKeyDB struct {
	err       error
	expiresAt *time.Time
}

func (d *scriptedKeyDB) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	return scriptedKeyRow{d}
}

type scriptedKeyRow struct{ d *scriptedKeyDB }

func (r scriptedKeyRow) Scan(dest ...any) error {
	if r.d.err != nil {
		return r.d.err
	}
	*dest[0].(*string) = "44444444-4444-4444-4444-444444444444"
	*dest[1].(*string) = "testnet"
	if len(dest) > 2 {
		*dest[2].(**time.Time) = r.d.expiresAt
	}
	return nil
}

func serveAuth(cfg DBAuthConfig) *httptest.ResponseRecorder {
	h := NewDBAuth(cfg)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	req.Header.Set("X-API-Key", "some-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	return body.Error.Code
}

// Issue #751: an infrastructure failure on the key lookup is a 503 the SDKs
// retry, not a 401 they surface as "invalid API key".
func TestNewDBAuth_DBTimeoutReturns503WithRetryAfter(t *testing.T) {
	before := testutil.ToFloat64(metrics.AuthKeyLookupFailuresTotal.WithLabelValues("unavailable"))

	rec := serveAuth(DBAuthConfig{DB: &scriptedKeyDB{err: context.DeadlineExceeded}})

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("missing Retry-After header")
	}
	if got := errCode(t, rec); got != "UNAVAILABLE" {
		t.Errorf("error code = %q, want UNAVAILABLE", got)
	}
	if got := testutil.ToFloat64(metrics.AuthKeyLookupFailuresTotal.WithLabelValues("unavailable")) - before; got != 1 {
		t.Errorf("unavailable counter delta = %v, want 1", got)
	}
}

func TestNewDBAuth_DBNoRowsReturns401(t *testing.T) {
	before := testutil.ToFloat64(metrics.AuthKeyLookupFailuresTotal.WithLabelValues("not_found"))
	unavailBefore := testutil.ToFloat64(metrics.AuthKeyLookupFailuresTotal.WithLabelValues("unavailable"))

	rec := serveAuth(DBAuthConfig{DB: &scriptedKeyDB{err: pgx.ErrNoRows}})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if got := testutil.ToFloat64(metrics.AuthKeyLookupFailuresTotal.WithLabelValues("not_found")) - before; got != 1 {
		t.Errorf("not_found counter delta = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.AuthKeyLookupFailuresTotal.WithLabelValues("unavailable")) - unavailBefore; got != 0 {
		t.Errorf("unavailable counter moved by %v on a plain not-found", got)
	}
}

func TestNewDBAuth_DBErrorStillAcceptsLegacyEnvKey(t *testing.T) {
	t.Setenv("API_KEY_SALT", "salt")
	t.Setenv("API_KEY_HASHES", hmacKeyHash("some-key"))

	rec := serveAuth(DBAuthConfig{DB: &scriptedKeyDB{err: errors.New("pool exhausted")}})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a valid legacy key during a DB outage", rec.Code)
	}
}

// Issue #752: a key past expires_at is rejected with a distinct code.
func TestNewDBAuth_ExpiredKeyRejectedWithDistinctCode(t *testing.T) {
	past := time.Now().Add(-time.Minute)
	before := testutil.ToFloat64(metrics.AuthKeyLookupFailuresTotal.WithLabelValues("expired"))

	rec := serveAuth(DBAuthConfig{DB: &scriptedKeyDB{expiresAt: &past}})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if got := errCode(t, rec); got != "API_KEY_EXPIRED" {
		t.Errorf("error code = %q, want API_KEY_EXPIRED", got)
	}
	if got := testutil.ToFloat64(metrics.AuthKeyLookupFailuresTotal.WithLabelValues("expired")) - before; got != 1 {
		t.Errorf("expired counter delta = %v, want 1", got)
	}
}

func TestNewDBAuth_FutureExpiryAccepted(t *testing.T) {
	future := time.Now().Add(time.Hour)
	if rec := serveAuth(DBAuthConfig{DB: &scriptedKeyDB{expiresAt: &future}}); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// The auth cache must not outlive the key: the entry's TTL is capped at the
// time remaining until expires_at, so the key is re-checked against the DB
// (and rejected) at expiry rather than up to authCacheTTL later.
func TestNewDBAuth_CacheTTLCappedAtExpiry(t *testing.T) {
	srv := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	soon := time.Now().Add(30 * time.Second)
	if rec := serveAuth(DBAuthConfig{DB: &scriptedKeyDB{expiresAt: &soon}, Redis: rdb}); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	ttl := srv.TTL(authRedisCacheKey(sha256KeyHash("some-key")))
	if ttl <= 0 || ttl > 30*time.Second {
		t.Fatalf("cache TTL = %v, want in (0, 30s]", ttl)
	}
}

func TestGraphQLDBAuth_ExpiredKeyRejected(t *testing.T) {
	past := time.Now().Add(-time.Minute)
	authFn := GraphQLDBAuth(DBAuthConfig{DB: &scriptedKeyDB{expiresAt: &past}})
	if _, _, ok := authFn(context.Background(), "some-key"); ok {
		t.Fatal("expired key authenticated on GraphQL")
	}
}

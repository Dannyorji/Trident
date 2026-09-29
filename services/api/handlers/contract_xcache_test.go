package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Depo-dev/trident/services/api/internal/contracttest"
	"github.com/Depo-dev/trident/services/api/middleware"
	"github.com/alicebob/miniredis/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/redis/go-redis/v9"
)

// wrapRateLimited mirrors main.go's real middleware chain closely enough
// for contract testing: GET /v1/stats/contracts's documented 200 response
// requires the X-RateLimit-* headers TieredRateLimit adds, which the bare
// handler under test doesn't set on its own (issue #242).
func wrapRateLimited(h http.Handler) http.Handler {
	cfg := middleware.RateLimitConfig{
		SliderFn: func(_ context.Context, _ string, limit, _ int64) (bool, int64, error) {
			return true, 1, nil
		},
		Tiers: map[string]middleware.TierConfig{"free": {RPS: 1000, Window: time.Second}},
	}
	return middleware.TieredRateLimit(cfg)(h)
}

type xcacheMissDB struct{}

func (xcacheMissDB) Ping(_ context.Context) error { return nil }
func (xcacheMissDB) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	return nil
}
func (xcacheMissDB) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return &noRowsResult{}, nil
}

// noRowsResult is a zero-row pgx.Rows stand-in for the ContractsStats
// live-aggregation query (issue #242's X-Cache MISS test) — no contracts are
// returned, only that the query executed and X-Cache: MISS was set.
type noRowsResult struct{ closed bool }

func (r *noRowsResult) Close()                                       { r.closed = true }
func (r *noRowsResult) Err() error                                   { return nil }
func (r *noRowsResult) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *noRowsResult) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *noRowsResult) Next() bool                                   { return false }
func (r *noRowsResult) Scan(_ ...any) error                          { return nil }
func (r *noRowsResult) Values() ([]any, error)                       { return nil, nil }
func (r *noRowsResult) RawValues() [][]byte                          { return nil }
func (r *noRowsResult) Conn() *pgx.Conn                              { return nil }

func newMiniredisClient(t *testing.T) *redis.Client {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	return redis.NewClient(&redis.Options{Addr: mr.Addr()})
}

// contractsStatsExplicitRangeReq builds a request with an explicit ledger
// range so ContractsStats takes the single-query live-aggregation path
// (queryContractStats) rather than the rollup fallback — keeps the DB mock
// trivial (issue #242). network is not a query parameter (issue #612): it is
// derived server-side from context, which defaults to "testnet" absent an
// authenticated key here, matching this helper's prior explicit value.
func contractsStatsExplicitRangeReq(t *testing.T) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/stats/contracts?from_ledger=0&to_ledger=1000000&limit=10", nil)
	req.URL.Scheme = "http"
	req.URL.Host = "localhost:3000"
	req.Host = "localhost:3000"
	req.Header.Set("X-API-Key", "contract-test-key")
	return req
}

// TestContractsStats_XCache_Miss verifies a cache-miss response sets
// X-Cache: MISS and conforms to GET /v1/stats/contracts's documented
// contract (issue #242).
func TestContractsStats_XCache_Miss(t *testing.T) {
	rdb := newMiniredisClient(t)
	req := contractsStatsExplicitRangeReq(t)

	rr := httptest.NewRecorder()
	wrapRateLimited(ContractsStats(xcacheMissDB{}, rdb)).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("X-Cache"); got != "MISS" {
		t.Errorf("X-Cache: want MISS, got %q", got)
	}

	doc := contracttest.LoadSpec(t)
	router := contracttest.NewRouter(t, doc)
	contracttest.ValidateResponse(t, router, req, rr.Code, rr.Header(), rr.Body.Bytes())
}

// TestContractsStats_XCache_Hit verifies a cache-hit response sets
// X-Cache: HIT, is served without touching the DB, and conforms to the same
// documented contract as a MISS (issue #242).
func TestContractsStats_XCache_Hit(t *testing.T) {
	rdb := newMiniredisClient(t)
	req := contractsStatsExplicitRangeReq(t)

	cacheKey := "stats:contracts:testnet:0:1000000:10:"
	cachedBody := `{"contracts":[],"from_ledger":0,"to_ledger":1000000,"network":"testnet","has_more":false,"next_cursor":null,"generated_at":"` +
		time.Now().UTC().Format(time.RFC3339) + `"}`
	if err := rdb.Set(context.Background(), cacheKey, cachedBody, time.Minute).Err(); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	// A DB that panics if queried — a HIT must never reach it.
	var panicsIfQueried DBPool = panicDB{t}

	rr := httptest.NewRecorder()
	wrapRateLimited(ContractsStats(panicsIfQueried, rdb)).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("X-Cache"); got != "HIT" {
		t.Errorf("X-Cache: want HIT, got %q", got)
	}

	doc := contracttest.LoadSpec(t)
	router := contracttest.NewRouter(t, doc)
	contracttest.ValidateResponse(t, router, req, rr.Code, rr.Header(), rr.Body.Bytes())
}

type panicDB struct{ t *testing.T }

func (p panicDB) Ping(_ context.Context) error { p.t.Fatal("unexpected Ping on cache HIT"); return nil }
func (p panicDB) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	p.t.Fatal("unexpected QueryRow on cache HIT")
	return nil
}
func (p panicDB) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	p.t.Fatal("unexpected Query on cache HIT")
	return nil, nil
}

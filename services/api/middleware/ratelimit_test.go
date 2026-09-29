package middleware

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Depo-dev/trident/services/api/internal/metrics"
	"github.com/alicebob/miniredis/v2"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
)

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

type mockTierDB struct {
	tier string
	err  error
}

func (m *mockTierDB) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	return &mockTierRow{tier: m.tier, err: m.err}
}

type mockTierRow struct {
	tier string
	err  error
}

func (r *mockTierRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*dest[0].(*string) = r.tier
	return nil
}

func noop() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func apiKeyReq(key string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	if key != "" {
		r.Header.Set("X-API-Key", key)
	}
	return r
}

func resetCounters() {
	rlAllowed.Store(0)
	rlRejected.Store(0)
}

// alwaysAllow is a SliderFn that always allows with count=1.
func alwaysAllow(_ context.Context, _ string, _ int64, _ int64) (bool, int64, error) {
	return true, 1, nil
}

// alwaysReject is a SliderFn that always rejects at the limit.
func alwaysReject(_ context.Context, _ string, limit int64, _ int64) (bool, int64, error) {
	return false, limit, nil
}

func testTiers() map[string]TierConfig {
	return map[string]TierConfig{
		"free":     {RPS: 5, Window: time.Second},
		"pro":      {RPS: 50, Window: time.Second},
		"internal": {RPS: 500, Window: time.Second},
	}
}

// ---------------------------------------------------------------------------
// TieredRateLimit tests
// ---------------------------------------------------------------------------

func TestTieredRateLimit_NoAPIKey_Passthrough(t *testing.T) {
	resetCounters()
	mw := TieredRateLimit(RateLimitConfig{Tiers: testTiers()})(noop())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, apiKeyReq(""))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if rec.Header().Get("X-RateLimit-Limit") != "" {
		t.Error("rate limit headers must not be set when no API key present")
	}
}

func TestTieredRateLimit_NilRedis_FailOpen(t *testing.T) {
	resetCounters()
	cfg := RateLimitConfig{Redis: nil, DB: nil, Tiers: testTiers()}
	mw := TieredRateLimit(cfg)(noop())
	for i := 0; i < 200; i++ {
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, apiKeyReq("any-key"))
		if rec.Code != http.StatusOK {
			t.Fatalf("nil Redis must fail open: want 200, got %d (i=%d)", rec.Code, i)
		}
	}
}

func TestTieredRateLimit_Allows_SetsHeaders(t *testing.T) {
	resetCounters()
	cfg := RateLimitConfig{SliderFn: alwaysAllow, Tiers: testTiers()}
	mw := TieredRateLimit(cfg)(noop())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, apiKeyReq("my-key"))

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if got := rec.Header().Get("X-RateLimit-Limit"); got != "5" {
		t.Errorf("X-RateLimit-Limit: want 5, got %q", got)
	}
	if rec.Header().Get("X-RateLimit-Remaining") == "" {
		t.Error("X-RateLimit-Remaining must be set")
	}
	if rec.Header().Get("X-RateLimit-Reset") == "" {
		t.Error("X-RateLimit-Reset must be set")
	}
	if rec.Header().Get("Retry-After") != "" {
		t.Error("Retry-After must not be set on 200")
	}
}

func TestTieredRateLimit_Rejects_Returns429WithHeaders(t *testing.T) {
	resetCounters()
	cfg := RateLimitConfig{SliderFn: alwaysReject, Tiers: testTiers()}
	mw := TieredRateLimit(cfg)(noop())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, apiKeyReq("key"))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("Retry-After must be set on 429")
	}
	if got := rec.Header().Get("X-RateLimit-Limit"); got != "5" {
		t.Errorf("X-RateLimit-Limit: want 5, got %q", got)
	}
	if got := rec.Header().Get("X-RateLimit-Remaining"); got != "0" {
		t.Errorf("X-RateLimit-Remaining: want 0, got %q", got)
	}
	if rec.Header().Get("X-RateLimit-Reset") == "" {
		t.Error("X-RateLimit-Reset must be set on 429")
	}
}

// TestTieredRateLimit_Rejects_RecordsPrometheusMetric verifies a 429 from the
// per-key tiered limiter increments trident_ratelimit_rejections_total{limiter="per_key"}
// (issue #58).
func TestTieredRateLimit_Rejects_RecordsPrometheusMetric(t *testing.T) {
	resetCounters()
	before := testutil.ToFloat64(metrics.RateLimitRejectionsTotal.WithLabelValues("per_key"))

	cfg := RateLimitConfig{SliderFn: alwaysReject, Tiers: testTiers()}
	mw := TieredRateLimit(cfg)(noop())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, apiKeyReq("key"))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %d", rec.Code)
	}
	if got := testutil.ToFloat64(metrics.RateLimitRejectionsTotal.WithLabelValues("per_key")); got != before+1 {
		t.Errorf("per_key rejections total: want %v, got %v", before+1, got)
	}
}

func TestTieredRateLimit_FailOpen_WhenRedisIsDown(t *testing.T) {
	resetCounters()
	var logBuf bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	metricBefore := testutil.ToFloat64(metrics.RateLimitFailOpenTotal.WithLabelValues("per_key"))

	client := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:0",
		DialTimeout: 50 * time.Millisecond,
		MaxRetries:  -1,
	})
	t.Cleanup(func() { _ = client.Close() })
	cfg := RateLimitConfig{Redis: client, Tiers: testTiers()}
	mw := TieredRateLimit(cfg)(noop())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, apiKeyReq("key"))
	if rec.Code != http.StatusOK {
		t.Fatalf("slider error must fail open: want 200, got %d", rec.Code)
	}
	if !strings.Contains(logBuf.String(), "rate limit check failed; failing open") {
		t.Fatalf("fail-open warning was not logged: %s", logBuf.String())
	}
	if got := testutil.ToFloat64(metrics.RateLimitFailOpenTotal.WithLabelValues("per_key")); got != metricBefore+1 {
		t.Errorf("fail-open metric: want %v, got %v", metricBefore+1, got)
	}
}

func TestTieredRateLimit_RealRedis_ConcurrentRequestsDoNotExceedTierLimit(t *testing.T) {
	resetCounters()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	const (
		limit    = 25
		requests = 200
	)
	mw := TieredRateLimit(RateLimitConfig{
		Redis: client,
		Tiers: map[string]TierConfig{"free": {RPS: limit, Window: time.Second}},
	})(noop())

	start := make(chan struct{})
	statuses := make([]int, requests)
	var wg sync.WaitGroup
	for i := range requests {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			rec := httptest.NewRecorder()
			mw.ServeHTTP(rec, apiKeyReq("concurrent-key"))
			statuses[i] = rec.Code
		}(i)
	}
	close(start)
	wg.Wait()

	allowed := 0
	for _, status := range statuses {
		switch status {
		case http.StatusOK:
			allowed++
		case http.StatusTooManyRequests:
		default:
			t.Fatalf("unexpected response status %d", status)
		}
	}
	if allowed != limit {
		t.Fatalf("concurrent aggregate allowed requests: want exactly %d, got %d", limit, allowed)
	}
}

func TestTieredRateLimit_TierFromDB_UsesCorrectLimit(t *testing.T) {
	resetCounters()
	db := &mockTierDB{tier: "internal"}
	var capturedLimit int64
	captureSlider := func(_ context.Context, _ string, limit, _ int64) (bool, int64, error) {
		capturedLimit = limit
		return true, 1, nil
	}
	cfg := RateLimitConfig{
		DB:       db,
		SliderFn: captureSlider,
		Tiers: map[string]TierConfig{
			"free":     {RPS: 5, Window: time.Second},
			"internal": {RPS: 500, Window: time.Second},
		},
	}
	mw := TieredRateLimit(cfg)(noop())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, apiKeyReq("internal-key"))
	if capturedLimit != 500 {
		t.Errorf("internal tier: want limit=500, got %d", capturedLimit)
	}
}

func TestTieredRateLimit_DBError_DefaultsToFree(t *testing.T) {
	resetCounters()
	db := &mockTierDB{err: fmt.Errorf("db down")}
	var capturedLimit int64
	captureSlider := func(_ context.Context, _ string, limit, _ int64) (bool, int64, error) {
		capturedLimit = limit
		return true, 1, nil
	}
	cfg := RateLimitConfig{
		DB:       db,
		SliderFn: captureSlider,
		Tiers: map[string]TierConfig{
			"free": {RPS: 10, Window: time.Second},
			"pro":  {RPS: 100, Window: time.Second},
		},
	}
	mw := TieredRateLimit(cfg)(noop())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, apiKeyReq("unknown-key"))
	if capturedLimit != 10 {
		t.Errorf("DB error should default to free (10), got %d", capturedLimit)
	}
}

func TestTieredRateLimit_Counters(t *testing.T) {
	resetCounters()
	call := 0
	var mu sync.Mutex
	countingSlider := func(_ context.Context, _ string, limit, _ int64) (bool, int64, error) {
		mu.Lock()
		defer mu.Unlock()
		call++
		if call <= 3 {
			return true, int64(call), nil
		}
		return false, limit, nil
	}
	cfg := RateLimitConfig{SliderFn: countingSlider, Tiers: testTiers()}
	mw := TieredRateLimit(cfg)(noop())

	for i := 0; i < 5; i++ {
		mw.ServeHTTP(httptest.NewRecorder(), apiKeyReq("ctr-key"))
	}

	allowed, rejected := RateLimitMetrics()
	if allowed != 3 {
		t.Errorf("allowed counter: want 3, got %d", allowed)
	}
	if rejected != 2 {
		t.Errorf("rejected counter: want 2, got %d", rejected)
	}
}

// TestTierCache_Invalidate_AppliesNewTierWithoutTTL asserts that after an admin
// tier change, invalidating the shared cache makes the new tier take effect on
// the next request instead of waiting for the 5-minute TTL (issue #229).
func TestTierCache_Invalidate_AppliesNewTierWithoutTTL(t *testing.T) {
	resetCounters()
	db := &mockTierDB{tier: "free"}
	cache := NewTierCache()

	var capturedLimit int64
	captureSlider := func(_ context.Context, _ string, limit, _ int64) (bool, int64, error) {
		capturedLimit = limit
		return true, 1, nil
	}
	cfg := RateLimitConfig{
		DB:       db,
		Cache:    cache,
		SliderFn: captureSlider,
		Tiers: map[string]TierConfig{
			"free": {RPS: 10, Window: time.Second},
			"pro":  {RPS: 100, Window: time.Second},
		},
	}
	mw := TieredRateLimit(cfg)(noop())
	const key = "switch-key"

	// First request resolves and caches the "free" tier.
	mw.ServeHTTP(httptest.NewRecorder(), apiKeyReq(key))
	if capturedLimit != 10 {
		t.Fatalf("initial tier: want limit 10 (free), got %d", capturedLimit)
	}

	// Admin promotes the key to "pro" in the DB. Without invalidation the cache
	// still serves the stale "free" tier.
	db.tier = "pro"
	mw.ServeHTTP(httptest.NewRecorder(), apiKeyReq(key))
	if capturedLimit != 10 {
		t.Fatalf("before invalidation: cached tier should still apply (10), got %d", capturedLimit)
	}

	// Invalidate the entry (as UpdateAPIKey does, passing the api_keys.key_hash
	// column value — plain SHA-256, not hashKey's HMAC) — the new tier applies now.
	cache.Invalidate(sha256KeyHash(key))
	mw.ServeHTTP(httptest.NewRecorder(), apiKeyReq(key))
	if capturedLimit != 100 {
		t.Fatalf("after invalidation: want new tier limit 100 (pro), got %d", capturedLimit)
	}
}

// spyTierDB records the hash argument passed to its lookup query so tests can
// assert TierCache.resolve queries by the same hash api_keys.key_hash actually
// stores (plain SHA-256), not some other digest.
type spyTierDB struct {
	tier       string
	gotHash    string
	queryCount int
}

func (m *spyTierDB) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	m.queryCount++
	if len(args) > 0 {
		if h, ok := args[0].(string); ok {
			m.gotHash = h
		}
	}
	return &mockTierRow{tier: m.tier}
}

// TestTierCache_Resolve_QueriesBySHA256NotHMAC is the regression test for
// issue #608: TieredRateLimit.resolve previously hashed the incoming API key
// with hashKey (HMAC-SHA256 + API_KEY_SALT) before querying
// "... WHERE key_hash = $1", but api_keys.key_hash is populated with plain
// SHA-256 (handlers.sha256hex, mirrored here by sha256KeyHash). Every
// DB-issued key's tier lookup therefore matched zero rows and silently fell
// back to "free", regardless of its real rate_limit_tier. This asserts the
// lookup uses the same hash the DB column and UpdateAPIKey's InvalidateTier
// call both use.
func TestTierCache_Resolve_QueriesBySHA256NotHMAC(t *testing.T) {
	db := &spyTierDB{tier: "pro"}
	cache := NewTierCache()
	const key = "sha-vs-hmac-key"

	tier := cache.resolve(context.Background(), key, db)

	if tier != "pro" {
		t.Fatalf("resolve: want tier %q from DB, got %q (silently fell back to free?)", "pro", tier)
	}
	want := sha256KeyHash(key)
	if db.gotHash != want {
		t.Fatalf("resolve queried key_hash = %q, want %q (sha256KeyHash, matching handlers.sha256hex and InvalidateTier's argument); got HMAC %q instead", db.gotHash, want, hashKey(key))
	}
	if db.gotHash == hashKey(key) {
		t.Fatal("resolve queried by hashKey's HMAC digest — this can never match a real api_keys.key_hash row")
	}
}

func TestTierCache_TierChangeAppliesWhenDocumentedTTLExpires(t *testing.T) {
	resetCounters()
	currentTime := time.Date(2026, time.August, 26, 12, 0, 0, 0, time.UTC)
	cache := NewTierCache()
	cache.now = func() time.Time { return currentTime }
	db := &mockTierDB{tier: "free"}

	var capturedLimit int64
	mw := TieredRateLimit(RateLimitConfig{
		DB:    db,
		Cache: cache,
		SliderFn: func(_ context.Context, _ string, limit, _ int64) (bool, int64, error) {
			capturedLimit = limit
			return true, 1, nil
		},
		Tiers: map[string]TierConfig{
			"free": {RPS: 10, Window: time.Second},
			"pro":  {RPS: 100, Window: time.Second},
		},
	})(noop())
	const key = "ttl-switch-key"

	mw.ServeHTTP(httptest.NewRecorder(), apiKeyReq(key))
	if capturedLimit != 10 {
		t.Fatalf("initial tier: want free limit 10, got %d", capturedLimit)
	}

	db.tier = "pro"
	currentTime = currentTime.Add(tierCacheTTL - time.Nanosecond)
	mw.ServeHTTP(httptest.NewRecorder(), apiKeyReq(key))
	if capturedLimit != 10 {
		t.Fatalf("before %s TTL: want cached free limit 10, got %d", tierCacheTTL, capturedLimit)
	}

	currentTime = currentTime.Add(time.Nanosecond)
	mw.ServeHTTP(httptest.NewRecorder(), apiKeyReq(key))
	if capturedLimit != 100 {
		t.Fatalf("at %s TTL: want refreshed pro limit 100, got %d", tierCacheTTL, capturedLimit)
	}
}

// TestTieredRateLimit_BoundaryExactLimit asserts the request that exactly hits
// the limit is allowed and the next one is rejected (issue #229 boundary case).
func TestTieredRateLimit_BoundaryExactLimit(t *testing.T) {
	resetCounters()
	const limit = 3
	var mu sync.Mutex
	calls := 0
	// Mimic the sliding window: allow while the pre-increment count is below the
	// limit, returning the post-increment count; reject once the window is full.
	slider := func(_ context.Context, _ string, lim, _ int64) (bool, int64, error) {
		mu.Lock()
		defer mu.Unlock()
		if int64(calls) >= lim {
			return false, lim, nil
		}
		calls++
		return true, int64(calls), nil
	}
	cfg := RateLimitConfig{
		SliderFn: slider,
		Tiers:    map[string]TierConfig{"free": {RPS: limit, Window: time.Second}},
	}
	mw := TieredRateLimit(cfg)(noop())
	const key = "boundary-key"

	// The first `limit` requests are allowed; the last exactly hits the cap.
	for i := 1; i <= limit; i++ {
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, apiKeyReq(key))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d/%d: want 200, got %d", i, limit, rec.Code)
		}
		if i == limit && rec.Header().Get("X-RateLimit-Remaining") != "0" {
			t.Errorf("at exact limit: want remaining 0, got %q", rec.Header().Get("X-RateLimit-Remaining"))
		}
		if i == limit && rec.Header().Get("X-RateLimit-Limit") != "3" {
			t.Errorf("at exact limit: want limit 3, got %q", rec.Header().Get("X-RateLimit-Limit"))
		}
		if i == limit && rec.Header().Get("Retry-After") != "" {
			t.Errorf("at exact limit: Retry-After must be absent, got %q", rec.Header().Get("Retry-After"))
		}
	}

	// The next request over the limit is rejected.
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, apiKeyReq(key))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over limit: want 429, got %d", rec.Code)
	}
	if got := rec.Header().Get("X-RateLimit-Limit"); got != "3" {
		t.Errorf("over limit: want limit 3, got %q", got)
	}
	if got := rec.Header().Get("X-RateLimit-Remaining"); got != "0" {
		t.Errorf("over limit: want remaining 0, got %q", got)
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("over limit: want Retry-After 1, got %q", got)
	}
	reset, err := strconv.ParseInt(rec.Header().Get("X-RateLimit-Reset"), 10, 64)
	if err != nil {
		t.Fatalf("over limit: invalid X-RateLimit-Reset: %v", err)
	}
	now := time.Now().Unix()
	if reset < now || reset > now+1 {
		t.Errorf("over limit: reset timestamp %d outside expected boundary [%d, %d]", reset, now, now+1)
	}
}

// ---------------------------------------------------------------------------
// WSConnectionLimit tests
// ---------------------------------------------------------------------------

func TestWSConnectionLimit_UnderLimit_Passes(t *testing.T) {
	wsConns.Store(0)
	defer wsConns.Store(0)
	mw := WSConnectionLimit(noop())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ws", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("under limit: want 200, got %d", rec.Code)
	}
}

func TestWSConnectionLimit_OverLimit_Returns429(t *testing.T) {
	wsConns.Store(1000) // already AT the limit
	defer wsConns.Store(0)
	mw := WSConnectionLimit(noop())
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ws", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over limit: want 429, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// RateLimitMetrics helper
// ---------------------------------------------------------------------------

func TestRateLimitMetrics_InitiallyZero(t *testing.T) {
	resetCounters()
	allowed, rejected := RateLimitMetrics()
	if allowed != 0 || rejected != 0 {
		t.Errorf("want (0,0), got (%d,%d)", allowed, rejected)
	}
}

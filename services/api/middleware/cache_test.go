package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Depo-dev/trident/services/api/middleware"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newCacheTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// waitForCacheVersion polls until key holds want, or fails the test.
//
// The deadline is generous on purpose. StartCacheInvalidator consumes the
// stream in its own goroutine, so a busy CI runner can schedule the XAdd well
// before the invalidator next reads — and when the poll gives up, t.Fatalf
// runs miniredis's t.Cleanup while that goroutine is still dialling, which
// surfaces as a confusing "dial tcp 127.0.0.1:0: connection refused" rather
// than the actual timeout. A short deadline here buys nothing: the happy path
// returns as soon as the value appears.
func waitForCacheVersion(t *testing.T, rdb *redis.Client, key, want string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var (
		v   string
		err error
	)
	for {
		v, err = rdb.Get(context.Background(), key).Result()
		if err == nil && v == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s was not bumped in time (last value=%q err=%v)", key, v, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// simpleJSONHandler returns a fixed body but lets the test see the call count.
func simpleJSONHandler(calls *int32, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}
}

func staticCacheKey(key, contractID string) middleware.CacheKeyFunc {
	return func(r *http.Request) (string, string) { return key, contractID }
}

func TestResponseCache_MissThenHit(t *testing.T) {
	rdb := newCacheTestRedis(t)
	var calls int32
	handler := simpleJSONHandler(&calls, `{"value":"a"}`)
	h := middleware.ResponseCache(rdb, time.Minute, staticCacheKey("k1", ""))(handler)

	req1 := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, req1)

	req2 := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)

	if rec1.Header().Get("X-Cache") != "MISS" {
		t.Errorf("first request X-Cache = %q, want MISS", rec1.Header().Get("X-Cache"))
	}
	if rec2.Header().Get("X-Cache") != "HIT" {
		t.Errorf("second request X-Cache = %q, want HIT", rec2.Header().Get("X-Cache"))
	}
	if rec1.Body.String() != rec2.Body.String() {
		t.Errorf("cached body differs: %q vs %q", rec1.Body.String(), rec2.Body.String())
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("handler executed %d times, want 1 (second request must be served from cache)", calls)
	}
}

func TestResponseCache_DistinctKeysDoNotShareCache(t *testing.T) {
	rdb := newCacheTestRedis(t)
	var calls int32
	handler := simpleJSONHandler(&calls, `{"value":"a"}`)

	h1 := middleware.ResponseCache(rdb, time.Minute, staticCacheKey("key-a", ""))(handler)
	h2 := middleware.ResponseCache(rdb, time.Minute, staticCacheKey("key-b", ""))(handler)

	h1.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	h2.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))

	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("handler executed %d times, want 2 (distinct keys must not share a cache slot)", calls)
	}
}

func TestResponseCache_NonGETBypassesCache(t *testing.T) {
	rdb := newCacheTestRedis(t)
	var calls int32
	handler := simpleJSONHandler(&calls, `{"value":"a"}`)
	h := middleware.ResponseCache(rdb, time.Minute, staticCacheKey("k1", ""))(handler)

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/x", nil)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	if atomic.LoadInt32(&calls) != 3 {
		t.Fatalf("handler executed %d times, want 3 (POST must never be cached)", calls)
	}
}

func TestResponseCache_EmptyKeyBypassesCache(t *testing.T) {
	rdb := newCacheTestRedis(t)
	var calls int32
	handler := simpleJSONHandler(&calls, `{"value":"a"}`)
	h := middleware.ResponseCache(rdb, time.Minute, staticCacheKey("", ""))(handler)

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("handler executed %d times, want 2 (empty key = not cacheable)", calls)
	}
}

func TestResponseCache_NilRedisPassesThrough(t *testing.T) {
	var calls int32
	handler := simpleJSONHandler(&calls, `{"value":"a"}`)
	h := middleware.ResponseCache(nil, time.Minute, staticCacheKey("k1", ""))(handler)

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("handler executed %d times, want 2 (nil redis = fail open, no caching)", calls)
	}
}

func TestResponseCache_OnlySuccessfulResponsesAreCached(t *testing.T) {
	rdb := newCacheTestRedis(t)
	var calls int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	})
	h := middleware.ResponseCache(rdb, time.Minute, staticCacheKey("k1", ""))(handler)

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("attempt %d: status = %d, want 500", i, rec.Code)
		}
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("handler executed %d times, want 2 (a 500 must never be cached and replayed)", calls)
	}
}

// TestResponseCache_SingleFlightCollapsesConcurrentMisses is the core
// stampede-protection acceptance criterion: N concurrent requests for the
// same cold key must produce exactly one execution of the handler.
func TestResponseCache_SingleFlightCollapsesConcurrentMisses(t *testing.T) {
	rdb := newCacheTestRedis(t)
	var calls int32
	release := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		<-release // hold every concurrent caller here until they've all arrived
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"value":"a"}`))
	})
	h := middleware.ResponseCache(rdb, time.Minute, staticCacheKey("stampede-key", ""))(handler)

	const n = 20
	var wg sync.WaitGroup
	results := make([]*httptest.ResponseRecorder, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			results[i] = rec
		}(i)
	}

	// Give every goroutine a chance to reach the handler (or the cache) before
	// releasing it — a generous but bounded wait, not a race-prone sleep tied
	// to correctness (the assertion below is what actually verifies it).
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("handler executed %d times across %d concurrent requests, want exactly 1", got, n)
	}
	for i, rec := range results {
		if rec.Code != http.StatusOK {
			t.Errorf("request %d: status = %d, want 200", i, rec.Code)
		}
	}
}

func TestResponseCache_InvalidationByContractVersionBump(t *testing.T) {
	rdb := newCacheTestRedis(t)
	var calls int32
	handler := simpleJSONHandler(&calls, `{"value":"a"}`)
	h := middleware.ResponseCache(rdb, time.Minute, staticCacheKey("spec-key", "CCONTRACT1"))(handler)

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("handler executed %d times before invalidation, want 1", calls)
	}

	// Simulate StartCacheInvalidator bumping this contract's version after a
	// new event — the same Redis operation it performs.
	if err := rdb.Incr(context.Background(), "cachever:CCONTRACT1").Err(); err != nil {
		t.Fatalf("incr cache version: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("X-Cache = %q after version bump, want MISS (must not still be serving the pre-invalidation entry)", rec.Header().Get("X-Cache"))
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("handler executed %d times after invalidation, want 2", calls)
	}
}

// -----------------------------------------------------------------------
// contractIDFromStreamMessage / StartCacheInvalidator
// -----------------------------------------------------------------------

func TestStartCacheInvalidator_BumpsVersionOnNewEvent(t *testing.T) {
	rdb := newCacheTestRedis(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const streamKey = "test:trident:events"
	go middleware.StartCacheInvalidator(ctx, rdb, streamKey)

	// StartCacheInvalidator's first XRead pins "$" (the stream's current
	// tail) at whatever moment that call actually reaches Redis, not at
	// goroutine-creation time — so writing the entry it should observe
	// immediately after `go ...`, with no gap, races the goroutine's own
	// startup. A brief wait lets that first blocking call land first; the
	// deadline loop below is the actual correctness check, not this delay.
	time.Sleep(50 * time.Millisecond)

	if err := rdb.XAdd(context.Background(), &redis.XAddArgs{
		Stream: streamKey,
		Values: map[string]any{"data": `{"contract_id":"CCONTRACT1","payload":{}}`},
	}).Err(); err != nil {
		t.Fatalf("XAdd: %v", err)
	}

	waitForCacheVersion(t, rdb, "cachever:CCONTRACT1", "1")
}

func TestStartCacheInvalidator_IgnoresMessagesWithoutContractID(t *testing.T) {
	rdb := newCacheTestRedis(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const streamKey = "test:trident:events:malformed"
	go middleware.StartCacheInvalidator(ctx, rdb, streamKey)

	// Malformed / missing-field messages must not panic the invalidator or
	// produce a spurious version key.
	for _, values := range []map[string]any{
		{"data": `not-json`},
		{"data": `{"payload":{}}`}, // no contract_id
		{"nope": "field"},
	} {
		if err := rdb.XAdd(context.Background(), &redis.XAddArgs{Stream: streamKey, Values: values}).Err(); err != nil {
			t.Fatalf("XAdd: %v", err)
		}
	}

	// A well-formed message afterwards must still work — proves the
	// goroutine kept running past the malformed entries rather than dying.
	if err := rdb.XAdd(context.Background(), &redis.XAddArgs{
		Stream: streamKey,
		Values: map[string]any{"data": `{"contract_id":"CAFTERBAD","payload":{}}`},
	}).Err(); err != nil {
		t.Fatalf("XAdd: %v", err)
	}

	waitForCacheVersion(t, rdb, "cachever:CAFTERBAD", "1")

	if exists, _ := rdb.Exists(context.Background(), "cachever:").Result(); exists != 0 {
		t.Error("an empty-string contract id must never produce a cachever: key")
	}
}

func TestStartCacheInvalidator_NilRedisReturnsImmediately(t *testing.T) {
	done := make(chan struct{})
	go func() {
		middleware.StartCacheInvalidator(context.Background(), nil, "whatever")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("StartCacheInvalidator with a nil client did not return")
	}
}

func TestDefaultCacheKey_VariesByPathNetworkAndQuery(t *testing.T) {
	req1 := httptest.NewRequest(http.MethodGet, "/v1/contracts/CABC/spec?b=2&a=1", nil)
	req1.SetPathValue("id", "CABC")
	key1, contractID1 := middleware.DefaultCacheKey(req1)

	req2 := httptest.NewRequest(http.MethodGet, "/v1/contracts/CABC/spec?a=1&b=2", nil)
	req2.SetPathValue("id", "CABC")
	key2, _ := middleware.DefaultCacheKey(req2)

	if key1 != key2 {
		t.Errorf("query parameter order changed the key: %q vs %q (must be normalised)", key1, key2)
	}
	if contractID1 != "CABC" {
		t.Errorf("contractID = %q, want %q", contractID1, "CABC")
	}

	// The regression this test exists for: two different contracts hitting
	// the *same registered route* ("/v1/contracts/{id}/spec") must never
	// collide on one cache entry just because the route pattern is shared.
	req3 := httptest.NewRequest(http.MethodGet, "/v1/contracts/COTHER/spec?a=1&b=2", nil)
	req3.SetPathValue("id", "COTHER")
	key3, contractID3 := middleware.DefaultCacheKey(req3)
	if key3 == key1 {
		t.Error("different contract ids produced the same cache key")
	}
	if contractID3 != "COTHER" {
		t.Errorf("contractID = %q, want %q", contractID3, "COTHER")
	}
}

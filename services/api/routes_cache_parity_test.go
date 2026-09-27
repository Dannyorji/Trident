package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Depo-dev/trident/services/api/middleware"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// cachedRoute describes one route registered with middleware.ResponseCache,
// carrying the exact TTL and key function routes.go wraps it with — the
// single source of truth for both tests in this file (issue #570).
//
// A middleware.ResponseCache(...) call added to routeBindings() without a
// matching entry here fails TestOnlyKnownRoutesAreResponseCacheWrapped
// below; an entry here for a route that stopped being cached fails the same
// test the other way. Neither drift direction passes silently, which is the
// "cannot rot" property #570 asks for.
type cachedRoute struct {
	method string
	path   string
	ttl    time.Duration
	keyFn  middleware.CacheKeyFunc
}

var cachedRoutes = []cachedRoute{
	{http.MethodGet, "/v1/contracts/{id}/spec", contractMetadataCacheTTL, middleware.DefaultCacheKey},
	{http.MethodGet, "/v1/contracts/{id}/metadata", contractMetadataCacheTTL, middleware.DefaultCacheKey},
}

// TestOnlyKnownRoutesAreResponseCacheWrapped is the fail-closed half of issue
// #570: nothing previously enforced that cachedRoutes above stays in sync
// with which routes routeBindings() actually wraps in
// middleware.ResponseCache. A route added to (or dropped from) the cache
// without updating this table now fails the build instead of silently
// drifting — the exact failure mode #570 exists to close ("the check cannot
// rot").
//
// Detection reuses routes_sideeffect_test.go's servesWithCacheHeader probe
// (issue #571): point at a closed Redis port so ResponseCache fails open,
// invoke the real handler with nil dependencies, and check for the X-Cache
// header ResponseCache stamps on every response it produces. A handler that
// panics on its nil deps never reached the middleware's tail — the deferred
// recover in servesWithCacheHeader still reports "not wrapped" correctly in
// that case.
func TestOnlyKnownRoutesAreResponseCacheWrapped(t *testing.T) {
	redis.SetLogger(quietRedisLogger{})
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	defer func() { _ = rdb.Close() }()

	want := make(map[string]bool, len(cachedRoutes))
	for _, c := range cachedRoutes {
		want[c.method+" "+c.path] = true
	}

	seen := make(map[string]bool)
	for _, b := range routeBindings() {
		if b.route.Method == "" {
			continue // /ws, /graphql: not GET routes, never cache candidates.
		}
		key := b.route.Method + " " + b.route.Path
		probe := b.handler(routeDeps{redisClient: rdb})
		if servesWithCacheHeader(probe) {
			seen[key] = true
		}
	}

	for key := range seen {
		if !want[key] {
			t.Errorf("%s is wrapped in middleware.ResponseCache but missing from cachedRoutes "+
				"in routes_cache_parity_test.go — add it and cover it with a header/status parity "+
				"assertion (issue #570)", key)
		}
	}
	for key := range want {
		if !seen[key] {
			t.Errorf("%s is listed in cachedRoutes but routeBindings() no longer wraps it in "+
				"middleware.ResponseCache — remove the stale entry (issue #570)", key)
		}
	}
}

// TestResponseCacheRoutes_HitPreservesHeadersAndStatus is the table-driven
// half of issue #570: for every route in cachedRoutes, a cache HIT must
// reproduce the exact headers and status the MISS produced, not just the
// body — the #221 defect that
// middleware.TestResponseCache_HitPreservesHeadersAndStatus
// (middleware/cache_headers_test.go) regression-tests for one hand-picked
// route. This runs the same assertion generically against every route's
// actual TTL and key function with a synthetic handler: the property under
// test is middleware.ResponseCache's replay behavior, which does not depend
// on any one route handler's business logic or its real dependencies
// (Postgres, schema registry, ...), so standing those up here would add
// weight without adding coverage.
func TestResponseCacheRoutes_HitPreservesHeadersAndStatus(t *testing.T) {
	for _, c := range cachedRoutes {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			server := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: server.Addr()})
			defer func() { _ = rdb.Close() }()

			var calls int
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Cache-Control", "public, max-age=60")
				w.Header().Set("X-Route-Specific", c.path)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"ok":true}`))
			})
			wrapped := middleware.ResponseCache(rdb, c.ttl, c.keyFn)(handler)

			do := func() *httptest.ResponseRecorder {
				req := httptest.NewRequest(c.method, "/v1/contracts/CTEST/x", nil)
				req.SetPathValue("id", "CTEST")
				rec := httptest.NewRecorder()
				wrapped.ServeHTTP(rec, req)
				return rec
			}

			miss := do()
			if got := miss.Header().Get("X-Cache"); got != "MISS" {
				t.Fatalf("first call X-Cache = %q, want MISS", got)
			}
			hit := do()
			if got := hit.Header().Get("X-Cache"); got != "HIT" {
				t.Fatalf("second call X-Cache = %q, want HIT", got)
			}
			if calls != 1 {
				t.Fatalf("handler ran %d times, want 1 (second call must be served from cache)", calls)
			}

			for _, hdr := range []string{"Cache-Control", "X-Route-Specific", "Content-Type"} {
				if miss.Header().Get(hdr) != hit.Header().Get(hdr) {
					t.Errorf("%s differs between MISS and HIT: %q vs %q",
						hdr, miss.Header().Get(hdr), hit.Header().Get(hdr))
				}
			}
			if hit.Code != miss.Code {
				t.Errorf("status differs: MISS %d, HIT %d", miss.Code, hit.Code)
			}
			if hit.Body.String() != miss.Body.String() {
				t.Errorf("body differs:\nMISS %q\nHIT  %q", miss.Body.String(), hit.Body.String())
			}
		})
	}
}

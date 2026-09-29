package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/redis/go-redis/v9"
)

// Routes whose handler performs a write and therefore must never sit behind
// middleware.ResponseCache, which states in its own doc comment that it "must
// never wrap a route with side effects".
//
// GET /v1/contracts/{id}/events/schema writes to contract_event_schemas on
// every call (persistContractSchemas), so a cache HIT silently skips that
// write for the rest of the TTL. That was fixed in #583, then reintroduced
// when #513 moved route registration into routes.go and re-wrapped the route,
// carrying ContractSpec's caching comment across with it. A comment did not
// prevent the recurrence; this test does.
var uncacheableRoutes = []struct{ method, path string }{
	{http.MethodGet, "/v1/contracts/{id}/events/schema"},
}

func TestSideEffectingRoutesAreNotCached(t *testing.T) {
	// Pointed at a closed port: ResponseCache's Redis lookup fails, it falls
	// open, runs the inner handler, and still stamps X-Cache: MISS on the way
	// out. An unwrapped handler never sets that header. No live Redis needed.
	// The dial failures are the point of the probe, not a problem to report:
	// silence go-redis' pool logger so a passing run stays readable.
	redis.SetLogger(quietRedisLogger{})
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	defer func() { _ = rdb.Close() }()

	bindings := routeBindings()

	for _, want := range uncacheableRoutes {
		var found bool
		for _, b := range bindings {
			if b.route.Method != want.method || b.route.Path != want.path {
				continue
			}
			found = true

			// Substitute a trivially successful handler for the real one so
			// the probe exercises the middleware chain the table builds, not
			// the handler's own dependencies (which are nil here).
			probe := b.handler(routeDeps{redisClient: rdb})
			if cached := servesWithCacheHeader(probe); cached {
				t.Errorf("%s %s is wrapped in ResponseCache, but its handler writes on "+
					"every call — a cache HIT skips that write for the whole TTL (issue #571)",
					want.method, want.path)
			}
		}
		if !found {
			t.Errorf("route %s %s not found in routeBindings(); update uncacheableRoutes if it moved",
				want.method, want.path)
		}
	}
}

// servesWithCacheHeader reports whether h emits the X-Cache header that
// middleware.ResponseCache stamps on every response it produces. A handler
// that panics on its nil dependencies never reached the middleware's tail, so
// a panic means "not wrapped" — ResponseCache captures the inner handler's
// response and sets the header itself before writing out.
func servesWithCacheHeader(h http.Handler) (cached bool) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/contracts/CTEST/events/schema", nil)

	defer func() {
		if r := recover(); r != nil {
			// Inner handler blew up on a nil DB before any wrapper could
			// finish. ResponseCache does not recover, so a panic reaching
			// here means the response never carried X-Cache.
			cached = rec.Header().Get("X-Cache") != ""
		}
	}()

	h.ServeHTTP(rec, req)
	return rec.Header().Get("X-Cache") != ""
}

// quietRedisLogger discards go-redis' internal pool diagnostics.
type quietRedisLogger struct{}

func (quietRedisLogger) Printf(_ context.Context, _ string, _ ...interface{}) {}

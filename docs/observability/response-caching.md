# Response Caching (issue #221)

A shared Redis response cache for the Go REST API's hot, read-only GET
endpoints, with per-route TTLs, a documented key strategy, stampede
protection, and event-driven invalidation for endpoints scoped to a single
contract.

## Key strategy

`middleware.DefaultCacheKey` (`services/api/middleware/cache.go`) builds a
key from three things a cacheable response can vary by:

```
<raw request path> | <authenticated network> | <query string, alphabetically sorted>
```

- **Raw path, not the registered route pattern.** `NewMetrics` and
  `StructuredLogging` deliberately collapse `/v1/contracts/{id}/spec` down to
  one label regardless of `{id}`, to bound a fixed label's cardinality — the
  opposite of what a cache key needs. Two different contracts must land in
  two different cache entries; using the route pattern here previously
  caused exactly that collision (fixed before this shipped, and covered by
  `TestDefaultCacheKey_VariesByPathNetworkAndQuery`).
- **Network**, from the authenticated API key's context
  (`middleware.NetworkFromContext`) — mainnet and testnet must never share a
  cache slot.
- **Query string, sorted.** `url.Values.Encode()` sorts keys alphabetically,
  so `?a=1&b=2` and `?b=2&a=1` hit the same cache entry.

A route can supply its own `CacheKeyFunc` instead when `DefaultCacheKey`'s
strategy doesn't fit (e.g. a listing endpoint that intentionally is not
scoped to a network).

## Stampede protection

`ResponseCache` wraps every request through a `golang.org/x/sync/singleflight.Group`
keyed on the same cache key. When N concurrent requests miss the same cold
key, only one actually executes the wrapped handler (and the one database
query it needs); the other N-1 block on `singleflight.Do` and receive that
same result, rather than each independently hammering the database.
`TestResponseCache_SingleFlightCollapsesConcurrentMisses` asserts the handler
runs exactly once under 20 concurrent misses.

Only a `200 OK` response is stored — a `4xx`/`5xx` is never cached and
replayed on the next request.

## `X-Cache`

Every cached route sets `X-Cache: HIT` or `X-Cache: MISS`, matching the
convention `handlers.ContractsStats` already established before this issue.
A request that shared a single-flighted miss with another concurrent request
still reports `MISS` — only one of the two genuinely queried the database,
but there is no meaningful third state worth the complexity of reporting.

## Invalidation

Cached entries also expire by TTL alone, but contract-scoped routes
(`GET /v1/contracts/{id}/spec`, `GET /v1/contracts/{id}/events/schema` — 5
minute TTL) don't have to wait that long: `middleware.StartCacheInvalidator`
does a best-effort, non-consumer-group `XRead` of the same `trident:events`
Redis Stream the indexer publishes to and the WebSocket hub consumes
(`ws.StreamKey`). For every event it sees, it increments `cachever:<contract_id>`.

`ResponseCache` folds that counter into the cache key it actually reads/writes
(`respcache:v<version>:<key>`), so bumping the counter doesn't require
deleting or even knowing every route/query combination cached for that
contract — the old entries are simply orphaned under the previous version
number and expire via their own TTL, and the very next request for that
contract computes a fresh response under the new version.

**Why not a durable consumer group** like `ws.StartConsumer`: that exists to
guarantee at-least-once delivery to WebSocket subscribers, which needs the
PEL/XACK/XAUTOCLAIM machinery. A missed invalidation here just means one
cache entry survives a little longer than ideal until its TTL expires — a
minor efficiency loss, not a correctness problem — so the simpler,
lower-overhead `XRead` loop is the right tool.

## Applying this to a new route

```go
mux.Handle("GET /v1/contracts/{id}/spec",
    middleware.ResponseCache(redisClient, ttl, middleware.DefaultCacheKey)(
        handlers.ContractSpec(schemaRegistryDB),
    ))
```

Only wrap routes that are:
- **GET, and free of side effects.** `ResponseCache` skips non-GET requests
  automatically, but it cannot know a GET handler has a side effect (e.g.
  `handlers.IndexerStats` also updates Prometheus gauges on every call) —
  don't wrap one of those, or the gauges go stale whenever the cache hits.
- **Not scoped to the individual caller.** Never cache a response that
  differs per-API-key (e.g. anything from `/v1/api-keys`) unless the key
  strategy also incorporates the caller's identity.

## Tests

`services/api/middleware/cache_test.go` covers: hit/miss and header
correctness, distinct keys never sharing an entry, non-GET and nil-Redis
bypass, only-200-is-cached, the single-flight stampede-collapse guarantee,
and version-bump invalidation. `StartCacheInvalidator` is covered against a
real (miniredis) Redis Stream: a well-formed event bumps the right
`cachever:` key, and malformed/contract-less messages are ignored without
killing the loop.

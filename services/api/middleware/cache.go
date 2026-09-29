package middleware

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

// cacheHandlerTimeout bounds a handler running detached from the winning
// request's context, so a stalled upstream cannot hold the singleflight key
// (and every waiter on it) open indefinitely.
const cacheHandlerTimeout = 30 * time.Second

// cachedResponse is the stored form of a cacheable response. The body alone
// is not enough: replaying without the handler's headers and status makes the
// same URL answer differently depending on whether the entry is warm.
type cachedResponse struct {
	Status int         `json:"status"`
	Header http.Header `json:"header"`
	Body   []byte      `json:"body"`
}

// cacheStreamEntry mirrors ws.streamEntry: the JSON structure the indexer
// writes as the "data" field of each Redis Stream message. Duplicated
// rather than imported — ws does not export its version, and this is a
// small, stable wire format not worth a shared-package dependency for.
type cacheStreamEntry struct {
	ContractID string `json:"contract_id"`
}

// contractIDFromStreamMessage extracts contract_id from one stream message's
// values, or "" if the message is missing, malformed, or carries no
// contract id — none of which are errors worth logging here: a message this
// invalidator can't parse just means its cache entries fall back to
// expiring by TTL instead of being invalidated early, and ws.StartConsumer
// already logs/handles the same malformed-message cases for the delivery
// path that actually needs them acknowledged.
func contractIDFromStreamMessage(values map[string]any) string {
	raw, ok := values["data"]
	if !ok {
		return ""
	}
	rawStr, ok := raw.(string)
	if !ok {
		return ""
	}
	var entry cacheStreamEntry
	if err := json.Unmarshal([]byte(rawStr), &entry); err != nil {
		return ""
	}
	return entry.ContractID
}

// CacheKeyFunc builds the cache key for a request and, when the response is
// scoped to one contract, that contract's id (empty otherwise). The
// contract id is used to invalidate the cache the moment a new event for
// that contract arrives (see StartCacheInvalidator), rather than only ever
// expiring by TTL.
type CacheKeyFunc func(r *http.Request) (key, contractID string)

// DefaultCacheKey builds a key from the request's raw path, the
// authenticated network, and the normalised (alphabetically sorted, via
// url.Values.Encode) query string — issue #221's "route + normalised query +
// network" strategy. contractID comes from the request's {id} path value,
// when the route has one; routes without an id path value (list/aggregate
// endpoints not scoped to a single contract) get an empty contractID and are
// never invalidated by new-event traffic, only by TTL.
//
// This deliberately uses the raw path (e.g. "/v1/contracts/CABC/spec"), not
// the registered route pattern ("/v1/contracts/{id}/spec") that
// NewMetrics/StructuredLogging use for their labels: those collapse path
// parameters together on purpose, to bound a fixed label's cardinality —
// exactly the wrong thing for a cache key, where two different contracts
// must land in two different cache entries, not share one.
//
// The three components are length-prefixed rather than joined on a bare
// separator (issue #576). Joining on an unescaped "|" let a component
// containing that byte imitate the delimiter: a path of
// "/v1/contracts/C1|mainnet|/spec" (reaching the server percent-encoded as
// %7C, which r.URL.Path holds decoded) produced a key byte-identical to the
// one built for a different path/network pair. Length-prefixing removes the
// ambiguity structurally — a reader consumes exactly n bytes, so no
// component's content can be mistaken for framing, whatever bytes it holds.
//
// This was not reachable when written: both routes wrapped today
// (/v1/contracts/{id}/spec and .../events/schema) validate the contract id
// against ^C[A-Z2-7]{55}$, which cannot contain "|". It is fixed as a trap
// for the next caller. ResponseCache is a general-purpose helper documented
// as such, and the next route wrapped with a free-form path segment or an
// unvalidated query parameter would get a cross-route cache collision whose
// symptom — one endpoint serving another endpoint's response — is very hard
// to trace back to a delimiter.
func DefaultCacheKey(r *http.Request) (string, string) {
	network := NetworkFromContext(r.Context())
	key := joinCacheKeyParts(r.URL.Path, network, r.URL.Query().Encode())
	return key, r.PathValue("id")
}

// joinCacheKeyParts joins cache key components unambiguously by prefixing
// each with its byte length: "<len>:<bytes>" repeated, e.g.
//
//	joinCacheKeyParts("/a", "mainnet", "") == "2:/a7:mainnet0:"
//
// Because a reader takes exactly the announced number of bytes, no component
// can forge the framing of another — which is the property a bare separator
// lacks and the whole point of issue #576. The encoding is injective: two
// different component tuples cannot produce the same string.
//
// The parts stay human-readable in the key, which matters when inspecting
// Redis during an incident; hashing would also close the collision but makes
// every key opaque.
func joinCacheKeyParts(parts ...string) string {
	var b strings.Builder
	// Each part contributes its bytes plus a short length prefix and colon.
	n := 0
	for _, p := range parts {
		n += len(p) + 8
	}
	b.Grow(n)
	for _, p := range parts {
		b.WriteString(strconv.Itoa(len(p)))
		b.WriteByte(':')
		b.WriteString(p)
	}
	return b.String()
}

// cacheVersionPrefix keys the per-contract invalidation counter (issue
// #221). Bumped by StartCacheInvalidator whenever a new event for that
// contract arrives; folding it into the cache key means invalidation never
// has to enumerate or delete every route/query combination cached for a
// contract — it just orphans them, and they expire via their own TTL.
func cacheVersionKey(contractID string) string {
	return "cachever:" + contractID
}

// ResponseCache returns middleware that caches a GET handler's JSON response
// in Redis for ttl (issue #221). Only GET requests are cacheable — this must
// never wrap a route with side effects: a cache HIT skips the wrapped
// handler entirely, so any write the handler makes (DB insert, external
// call, etc.) silently stops happening for the rest of the TTL once an entry
// fills. This is a contract enforced by the caller at the mux.Handle call
// site (see main.go), not by this middleware — nothing here inspects the
// wrapped handler for side effects, so verify it by hand before adding a new
// ResponseCache-wrapped route (issue #571).
//
// Concurrent requests for the same not-yet-cached key are collapsed via
// singleflight: only one actually executes the wrapped handler, the rest
// wait for and share its result, so a burst of traffic for a cold key
// produces one database query, not N ("single-flight to avoid stampedes").
// Every response — hit, miss, or one that shared a single-flighted miss —
// carries X-Cache: HIT or MISS.
//
// A nil rdb, or any request whose keyFn reports no cacheable key, passes
// straight through uncached — the same fail-open posture the rest of this
// service takes when Redis is unavailable.
func ResponseCache(rdb *redis.Client, ttl time.Duration, keyFn CacheKeyFunc) func(http.Handler) http.Handler {
	var group singleflight.Group
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || rdb == nil {
				next.ServeHTTP(w, r)
				return
			}

			key, contractID := keyFn(r)
			if key == "" {
				next.ServeHTTP(w, r)
				return
			}

			version := "0"
			if contractID != "" {
				if v, err := rdb.Get(r.Context(), cacheVersionKey(contractID)).Result(); err == nil {
					version = v
				}
			}
			fullKey := fmt.Sprintf("respcache:v%s:%s", version, key)

			if cached, err := rdb.Get(r.Context(), fullKey).Result(); err == nil {
				// Replay the stored headers and status, not just the body.
				// Storing the body alone meant every header the handler set
				// (Cache-Control, ETag, anything route-specific) appeared on
				// the MISS and was silently gone once the entry was warm — the
				// same URL answering differently depending on cache state.
				var rec cachedResponse
				if jsonErr := json.Unmarshal([]byte(cached), &rec); jsonErr == nil {
					for k, vv := range rec.Header {
						for _, v := range vv {
							w.Header().Add(k, v)
						}
					}
					if w.Header().Get("Content-Type") == "" {
						w.Header().Set("Content-Type", "application/json")
					}
					w.Header().Set("X-Cache", "HIT")
					status := rec.Status
					if status == 0 {
						status = http.StatusOK
					}
					w.WriteHeader(status)
					_, _ = w.Write(rec.Body)
					return
				}
				// A malformed entry is treated as a miss: re-executing costs
				// a query, serving garbage costs correctness.
				slog.WarnContext(r.Context(), "responsecache: malformed cache entry; re-executing", "key", fullKey)
			}

			// singleflight.Do's function runs on whichever goroutine's call
			// wins the race for this key; the rest block here and receive
			// its return value directly, never re-executing next.
			result, _, _ := group.Do(fullKey, func() (any, error) {
				capture := newCaptureWriter()

				// Run the handler on a context detached from the winning
				// request's cancellation. Without this, if the goroutine that
				// happened to win the race has its client disconnect
				// mid-flight, the handler aborts and EVERY waiter receives
				// that aborted response — unrelated healthy clients failing
				// because a stranger hung up. Only the values (auth, request
				// id, network) are carried over, not the cancellation.
				//
				// The 30s ceiling keeps a detached handler from outliving the
				// request set that is waiting on it.
				runCtx, cancel := context.WithTimeout(
					context.WithoutCancel(r.Context()), cacheHandlerTimeout)
				defer cancel()

				next.ServeHTTP(capture, r.WithContext(runCtx))
				if capture.statusCode == http.StatusOK {
					// context.Background(): this write must complete even if
					// the request that happened to win the singleflight race
					// has its own context cancelled (client disconnect)
					// before the waiting callers have read the result.
					if data, err := json.Marshal(cachedResponse{
						Status: capture.statusCode,
						Header: capture.header,
						Body:   capture.body.Bytes(),
					}); err == nil {
						_ = rdb.Set(context.Background(), fullKey, data, ttl).Err()
					}
				}
				return capture, nil
			})

			// Every goroutine singleflight.Do released — the one that ran
			// next.ServeHTTP and every one that waited for it — shares this
			// same *captureWriter. Reading it is safe (it is fully built
			// before Do returns to anyone), but mutating it here is not:
			// with N concurrent callers doing so, that is exactly the data
			// race between the code that used to call
			// capture.header.Set(...) from every goroutine. So each caller
			// only reads capture and writes to its own w — never the other
			// way around.
			capture := result.(*captureWriter)
			for k, vv := range capture.header {
				for _, v := range vv {
					w.Header().Add(k, v)
				}
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Cache", "MISS")
			w.WriteHeader(capture.statusCode)
			_, _ = w.Write(capture.body.Bytes())
		})
	}
}

// StartCacheInvalidator subscribes (best-effort, no consumer group) to the
// Redis Stream the indexer publishes events to, and bumps each event's
// contract's cache version (issue #221) so ResponseCache-wrapped endpoints
// stop serving pre-event responses immediately rather than waiting out the
// full TTL.
//
// Deliberately not a durable consumer group like ws.StartConsumer: a missed
// message here just means an affected cache entry survives until its TTL
// expires instead of being invalidated early — a minor efficiency loss, not
// a correctness or delivery guarantee this feature needs. Consumer-group
// bookkeeping (PEL, XACK, XAUTOCLAIM) exists to guarantee at-least-once
// delivery to WebSocket subscribers, which has no analogue here.
//
// Returns once ctx is cancelled.
func StartCacheInvalidator(ctx context.Context, rdb *redis.Client, streamKey string) {
	if rdb == nil {
		return
	}
	lastID := "$" // start from "new messages only", like ws.StartConsumer's initial XGroupCreate position.
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		entries, err := rdb.XRead(ctx, &redis.XReadArgs{
			Streams: []string{streamKey, lastID},
			Block:   5 * time.Second,
			Count:   100,
		}).Result()
		if err != nil {
			if err == redis.Nil {
				continue // block timeout with nothing new — expected, not an error.
			}
			if ctx.Err() != nil {
				return
			}
			time.Sleep(time.Second)
			continue
		}

		for _, stream := range entries {
			for _, msg := range stream.Messages {
				lastID = msg.ID
				contractID := contractIDFromStreamMessage(msg.Values)
				if contractID != "" {
					rdb.Incr(ctx, cacheVersionKey(contractID))
				}
			}
		}
	}
}

package middleware

import (
	"context"
	"fmt"
	"time"
)

// Auth and rate limiting for the GraphQL/WS transport (issue #223).
//
// Why this exists rather than reusing the HTTP middleware directly
// ----------------------------------------------------------------
// Both HTTP middlewares key off request headers, and a graphql-transport-ws
// client presents its credential in the connection_init payload after the
// upgrade instead. That left two real gaps on /graphql:
//
//   - NewDBAuth returns early for any path that is neither /v1/* nor /ws, so
//     it never ran for /graphql at all. The endpoint authenticated only
//     against the legacy API_KEY_HASHES env-var set, a different key store
//     from the DB-backed api_keys table REST uses — a key could be valid on
//     one transport and unknown on the other, and revoking a row in api_keys
//     did not close a GraphQL connection's access.
//
//   - TieredRateLimit reads the X-API-Key header and passes the request
//     straight through when it is absent. A WebSocket client never sends
//     that header, so GraphQL operations were entirely unmetered while a
//     REST caller holding the same key was throttled.
//
// These adapters resolve both against the same api_keys table and the same
// tier configuration and sliding window the HTTP path uses, so a key gets one
// identity and one budget across both transports.

// GraphQLDBAuth returns an authenticator for the GraphQL transport that
// resolves a raw API key against the same api_keys table NewDBAuth queries,
// through the same Redis cache, and returns the key's id and network.
//
// The network is what scopes every read on the connection, mirroring how the
// REST handlers take it from the authenticated key context rather than from
// the request — so a caller cannot reach another network's data by naming it
// in a query.
//
// When cfg has neither a database nor a Redis cache, authentication is not
// configured and every key is accepted on the default network. That matches
// the posture Auth takes with an empty hash set and keeps local development
// working without a database.
func GraphQLDBAuth(cfg DBAuthConfig) func(ctx context.Context, key string) (string, string, bool) {
	return func(ctx context.Context, key string) (string, string, bool) {
		if cfg.DB == nil && cfg.Redis == nil {
			return "", "testnet", true
		}
		if key == "" {
			return "", "", false
		}

		dbHash := sha256KeyHash(key)

		// Redis cache first, in the same "<uuid>:<network>" format NewDBAuth
		// writes, so the two transports share cache entries instead of each
		// warming its own.
		if cfg.Redis != nil {
			if cached, err := cfg.Redis.Get(ctx, authRedisCacheKey(dbHash)).Result(); err == nil {
				id, network := splitCachedAuth(cached)
				if network == "" {
					network = "testnet"
				}
				return id, network, true
			}
		}

		if cfg.DB == nil {
			return "", "", false
		}

		dbCtx, cancel := context.WithTimeout(ctx, authDBQueryTimeout)
		defer cancel()

		var id, network string
		err := cfg.DB.QueryRow(dbCtx,
			`SELECT id, network FROM api_keys WHERE key_hash = $1 AND revoked_at IS NULL`,
			dbHash,
		).Scan(&id, &network)
		if err != nil {
			return "", "", false
		}

		if cfg.Redis != nil {
			cfg.Redis.Set(ctx, authRedisCacheKey(dbHash), id+":"+network, authCacheTTL)
		}
		if network == "" {
			network = "testnet"
		}
		return id, network, true
	}
}

// splitCachedAuth parses NewDBAuth's "<uuid>:<network>" cache value.
func splitCachedAuth(cached string) (id, network string) {
	for i := 0; i < len(cached); i++ {
		if cached[i] == ':' {
			return cached[:i], cached[i+1:]
		}
	}
	return cached, ""
}

// GraphQLRateLimiter returns a per-operation limiter for the GraphQL
// transport that consumes from the same sliding window, under the same Redis
// key, as the HTTP tiered limiter.
//
// Sharing the window is the point: it means a client cannot double its
// throughput by splitting traffic across REST and GraphQL, because both
// decrement one budget. The tier is resolved per key exactly as
// TieredRateLimit resolves it.
//
// keyID is the api_keys row id established at connection_init. An empty
// keyID (authentication not configured) is always allowed, matching what the
// HTTP limiter does for a request with no key.
func GraphQLRateLimiter(cfg RateLimitConfig) func(ctx context.Context, keyID string) (bool, error) {
	if cfg.Tiers == nil {
		cfg.Tiers = defaultTiers()
	}
	slide := cfg.SliderFn
	if slide == nil {
		if cfg.Redis != nil {
			slide = redisSlider(cfg.Redis)
		} else {
			// No Redis: the HTTP limiter degrades to allow-all here too.
			slide = func(_ context.Context, _ string, _, _ int64) (bool, int64, error) {
				return true, 0, nil
			}
		}
	}
	tc := cfg.Cache
	if tc == nil {
		tc = NewTierCache()
	}

	return func(ctx context.Context, keyID string) (bool, error) {
		if keyID == "" {
			return true, nil
		}

		tier := tc.resolve(ctx, keyID, cfg.DB)
		tcfg, ok := cfg.Tiers[tier]
		if !ok {
			tcfg = cfg.Tiers["free"]
		}

		redisKey := fmt.Sprintf("ratelimit:%s", hashKey(keyID))
		windowMs := int64(tcfg.Window / time.Millisecond)
		limit := int64(tcfg.RPS)

		allowed, _, err := slide(ctx, redisKey, limit, windowMs)
		if err != nil {
			// Fail open, as TieredRateLimit does: a Redis blip must not take
			// GraphQL down while REST keeps serving.
			return true, err
		}
		return allowed, nil
	}
}

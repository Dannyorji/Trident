package main

import (
	"time"

	"database/sql"
	"net/http"

	"github.com/Depo-dev/trident/services/api/grpc"
	"github.com/Depo-dev/trident/services/api/handlers"
	"github.com/Depo-dev/trident/services/api/middleware"
	"github.com/Depo-dev/trident/services/api/ws"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// routeDeps carries everything the route handlers need. Built once in main();
// the OpenAPI inventory contract test never constructs one — it reads the
// route table through routeInventory(), which touches no handler (issue #513).
type routeDeps struct {
	pool             *pgxpool.Pool
	healthDB         handlers.DBPool
	schemaRegistryDB handlers.SchemaRegistryDB
	redisClient      *redis.Client
	grpcClient       *grpc.Client
	adminCfg         handlers.AdminConfig
	contractCfg      handlers.ContractConfig
	apiKeyCfg        handlers.APIKeyConfig
	sorobanCaller    handlers.SorobanRPCCaller
	webhookDB        *sql.DB
	hub              *ws.Hub
	rlCfg            middleware.RateLimitConfig
	authDB           middleware.DBAuthConfig
}

const contractMetadataCacheTTL = 5 * time.Minute

// RegisteredRoute describes one mux registration, for the OpenAPI inventory
// contract test (issue #513): every route is either documented in
// api/openapi.yaml or carries an explicit exemption reason. There is no third
// state — a new route added without deciding gets caught by the test.
type RegisteredRoute struct {
	// Method is empty for registrations that accept any method (/ws, /graphql).
	Method string
	Path   string
	// Documented routes must have a matching operation in api/openapi.yaml;
	// undocumented routes must state why they are excluded from the public
	// v1 surface.
	Documented      bool
	ExemptionReason string
}

// routeBinding pairs a route's inventory entry with a lazily constructed
// handler. The closure is only invoked by registerRoutes with real
// dependencies, so enumerating the table (routeInventory) is side-effect
// free — the single slice literal below is simultaneously the registration
// source of truth and the contract-test inventory, which is what makes
// route↔spec drift structurally impossible to reintroduce.
type routeBinding struct {
	route   RegisteredRoute
	handler func(d routeDeps) http.Handler
}

func documented(method, path string, h func(d routeDeps) http.Handler) routeBinding {
	return routeBinding{
		route:   RegisteredRoute{Method: method, Path: path, Documented: true},
		handler: h,
	}
}

func internalOnly(method, path, reason string, h func(d routeDeps) http.Handler) routeBinding {
	return routeBinding{
		route: RegisteredRoute{
			Method:          method,
			Path:            path,
			Documented:      false,
			ExemptionReason: reason,
		},
		handler: h,
	}
}

func routeBindings() []routeBinding {
	return []routeBinding{
		documented("GET", "/v1/health", func(d routeDeps) http.Handler { return handlers.Health() }),
		documented("GET", "/v1/ready", func(d routeDeps) http.Handler {
			// A nil *grpc.Client assigned into the EventsLister interface
			// parameter is NOT an interface nil - client == nil inside
			// checkGRPC would be false, and the health check would panic
			// calling a method on a nil receiver instead of reporting
			// errNoGRPCClient. Passing a genuine nil interface value when
			// the concrete pointer is nil keeps Ready's own nil check
			// meaningful for callers (like tests) that construct routeDeps
			// without a real gRPC client.
			var eventsLister handlers.EventsLister
			if d.grpcClient != nil {
				eventsLister = d.grpcClient
			}
			return handlers.Ready(d.healthDB, d.redisClient, eventsLister)
		}),
		documented("GET", "/v1/version", func(d routeDeps) http.Handler {
			return handlers.VersionHandler(d.pool)
		}),
		documented("GET", "/v1/events", func(d routeDeps) http.Handler {
			return http.HandlerFunc(handlers.ListEvents)
		}),
		documented("POST", "/v1/events/batch", func(d routeDeps) http.Handler {
			return http.HandlerFunc(handlers.BatchGetEvents)
		}),
		documented("GET", "/v1/events/{id}", func(d routeDeps) http.Handler {
			return http.HandlerFunc(handlers.GetEvent)
		}),
		documented("GET", "/v1/events/stream", func(d routeDeps) http.Handler {
			return handlers.Stream(d.redisClient)
		}),
		documented("GET", "/v1/admin/db", func(d routeDeps) http.Handler {
			return handlers.AdminDB(d.adminCfg)
		}),
		documented("GET", "/v1/admin/keys/{id}/usage", func(d routeDeps) http.Handler {
			return handlers.AdminKeyUsage(d.adminCfg)
		}),
		documented("GET", "/v1/admin/keys/{id}/usage-rollup", func(d routeDeps) http.Handler {
			return handlers.AdminKeyUsageRollup(d.adminCfg)
		}),
		documented("GET", "/v1/usage", func(d routeDeps) http.Handler {
			return handlers.KeyUsage(handlers.UsageConfig{DB: d.pool})
		}),
		// Admin contract registration CRUD (issue #230)
		documented("POST", "/v1/admin/contracts", func(d routeDeps) http.Handler {
			return handlers.CreateContract(d.contractCfg)
		}),
		documented("GET", "/v1/admin/contracts", func(d routeDeps) http.Handler {
			return handlers.ListContracts(d.contractCfg)
		}),
		documented("DELETE", "/v1/admin/contracts/{id}", func(d routeDeps) http.Handler {
			return handlers.DeleteContract(d.contractCfg)
		}),
		// API key management (admin-only via X-Admin-Key header)
		documented("POST", "/v1/api-keys", func(d routeDeps) http.Handler {
			// Idempotency wraps only the create route (issue #225).
			return middleware.Idempotency(d.redisClient, middleware.DefaultIdempotencyTTL)(
				handlers.CreateAPIKey(d.apiKeyCfg))
		}),
		documented("GET", "/v1/api-keys", func(d routeDeps) http.Handler {
			return handlers.ListAPIKeys(d.apiKeyCfg)
		}),
		// Atomic rotation: mints a replacement key and evicts the old one's
		// auth cache entry in the same request (issue #516).
		documented("POST", "/v1/api-keys/{id}/rotate", func(d routeDeps) http.Handler {
			return handlers.RotateAPIKey(d.apiKeyCfg)
		}),
		documented("PATCH", "/v1/api-keys/{id}", func(d routeDeps) http.Handler {
			return handlers.UpdateAPIKey(d.apiKeyCfg)
		}),
		documented("DELETE", "/v1/api-keys/{id}", func(d routeDeps) http.Handler {
			return handlers.DeleteAPIKey(d.apiKeyCfg)
		}),
		documented("GET", "/v1/stats/indexer", func(d routeDeps) http.Handler {
			return handlers.IndexerStats(d.healthDB)
		}),
		// ContractEventSchemas is intentionally NOT wrapped in ResponseCache
		// (issue #571): unlike ContractSpec, it writes to
		// contract_event_schemas on every call (persistContractSchemas), and
		// ResponseCache "must never wrap a route with side effects" — a cache
		// HIT would skip that write for the rest of the TTL. Its queries are
		// indexed lookups against token_events/soroban_events, cheap enough
		// that going uncached does not need a cache of its own.
		documented("GET", "/v1/contracts/{id}/events/schema", func(d routeDeps) http.Handler {
			return handlers.ContractEventSchemas(d.schemaRegistryDB)
		}),
		// ContractSpec changes only when a contract is redeployed — rare,
		// read-only, no side effects — so it is cached (issue #221) with a TTL
		// well above the 60s used for stats, invalidated immediately on a new
		// event for that contract (see StartCacheInvalidator).
		documented("GET", "/v1/contracts/{id}/spec", func(d routeDeps) http.Handler {
			return middleware.ResponseCache(d.redisClient, contractMetadataCacheTTL,
				middleware.DefaultCacheKey)(handlers.ContractSpec(d.schemaRegistryDB))
		}),
		// SEP-41 token metadata resolved by the indexer (issue #263). Its
		// registration was dropped when routes moved out of main.go, leaving
		// the handler unreachable and TokenMetadataResponse unused in the spec.
		documented("GET", "/v1/contracts/{id}/metadata", func(d routeDeps) http.Handler {
			return middleware.ResponseCache(d.redisClient, contractMetadataCacheTTL,
				middleware.DefaultCacheKey)(handlers.TokenMetadata(d.pool))
		}),
		documented("GET", "/v1/contracts/{id}/storage", func(d routeDeps) http.Handler {
			return handlers.ContractStorageLatest(d.schemaRegistryDB)
		}),
		documented("GET", "/v1/contracts/{id}/storage/history", func(d routeDeps) http.Handler {
			return handlers.ContractStorageHistory(d.schemaRegistryDB)
		}),
		documented("GET", "/v1/stats/contracts", func(d routeDeps) http.Handler {
			return handlers.ContractsStats(d.pool, d.redisClient)
		}),
		documented("POST", "/v1/contracts/{id}/call", func(d routeDeps) http.Handler {
			return handlers.CallContract(d.sorobanCaller)
		}),
		documented("GET", "/v1/webhooks", func(d routeDeps) http.Handler {
			return listWebhooksHandler(d.webhookDB)
		}),
		documented("POST", "/v1/webhooks", func(d routeDeps) http.Handler {
			// Idempotency wraps only the create route (issue #225).
			return middleware.Idempotency(d.redisClient, middleware.DefaultIdempotencyTTL)(
				createWebhookHandler(d.webhookDB))
		}),
		documented("POST", "/v1/webhooks/{id}/rotate-secret", func(d routeDeps) http.Handler {
			return rotateWebhookSecretHandler(d.webhookDB)
		}),
		documented("DELETE", "/v1/webhooks/{id}", func(d routeDeps) http.Handler {
			return deleteWebhookHandler(d.webhookDB)
		}),
		documented("PATCH", "/v1/webhooks/{id}/pause", func(d routeDeps) http.Handler {
			return pauseWebhookHandler(d.webhookDB)
		}),
		documented("PATCH", "/v1/webhooks/{id}/resume", func(d routeDeps) http.Handler {
			return resumeWebhookHandler(d.webhookDB)
		}),
		documented("GET", "/v1/webhooks/{id}/deliveries", func(d routeDeps) http.Handler {
			return deliveriesWebhookHandler(d.webhookDB)
		}),
		documented("GET", "/v1/webhooks/{id}/dead-letters", func(d routeDeps) http.Handler {
			return deadLettersWebhookHandler(d.webhookDB)
		}),
		documented("POST", "/v1/webhooks/{id}/dead-letters/{deliveryId}/replay", func(d routeDeps) http.Handler {
			return replayDeadLetterHandler(d.webhookDB)
		}),
		documented("GET", "/metrics", func(d routeDeps) http.Handler {
			return handlers.MetricsHandler(d.pool, d.redisClient)
		}),
		internalOnly("GET", "/internal/status", "operator-facing internals, not part of the public v1 surface",
			func(d routeDeps) http.Handler { return handlers.InternalStatus() }),
		internalOnly("", "/ws", "WebSocket upgrade endpoint; documented in the WebSocket guide, not representable as an OpenAPI operation",
			func(d routeDeps) http.Handler { return middleware.WSConnectionLimit(ws.Handler(d.hub)) }),
		internalOnly("", "/graphql", "GraphQL-over-WebSocket endpoint; carries its own schema, documented in the GraphQL guide",
			func(d routeDeps) http.Handler {
				// GraphQL reuses the REST surface's auth and rate-limit
				// config (issue #223): the HTTP middlewares cannot cover
				// this endpoint on their own — NewDBAuth skips any path
				// that is neither /v1/* nor /ws, and TieredRateLimit keys
				// off the X-API-Key header, which a WebSocket client never
				// sends (it authenticates in the connection_init payload).
				return middleware.WSConnectionLimit(ws.GraphQLHandler(d.hub, ws.GraphQLDeps{
					Auth:        middleware.GraphQLDBAuth(d.authDB),
					RateLimiter: middleware.GraphQLRateLimiter(d.rlCfg),
					Backend:     handlers.NewGraphQLBackend(d.pool),
				}))
			}),
	}
}

// registerRoutes mounts every route on the mux. main() is its only caller.
func registerRoutes(mux *http.ServeMux, d routeDeps) {
	for _, b := range routeBindings() {
		pattern := b.route.Path
		if b.route.Method != "" {
			pattern = b.route.Method + " " + b.route.Path
		}
		mux.Handle(pattern, b.handler(d))
	}
}

// routeInventory exposes the route table for the OpenAPI inventory contract
// test without constructing any handler or dependency.
func routeInventory() []RegisteredRoute {
	bindings := routeBindings()
	out := make([]RegisteredRoute, 0, len(bindings))
	for _, b := range bindings {
		out = append(out, b.route)
	}
	return out
}

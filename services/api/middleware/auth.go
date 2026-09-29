package middleware

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Depo-dev/trident/services/api/internal/httputil"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
)

// DBAuthConfig configures the database-backed authentication middleware.
type DBAuthConfig struct {
	// DB is used for API key lookups. When nil, only env-var auth is attempted.
	DB interface {
		QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	}
	// Redis is used for caching successful lookups (5 min TTL). Optional.
	Redis *redis.Client
	// UsageTrack receives the authenticated key's id on every successful
	// DB-backed or cached auth (issue #615), feeding
	// handlers.NewAPIKeyUsageTracker's batched request_count/last_used_at
	// flush. Optional: nil disables tracking (matches main.go's existing
	// "only start the tracker when pool != nil" behavior).
	UsageTrack chan<- string
}

// trackUsage sends idStr on track without blocking the request: the channel
// is large (4096) and drained every few seconds, but a request must never
// wait on it, and a full channel must never be treated as an error, usage
// tracking is explicitly non-critical (NewAPIKeyUsageTracker's own doc
// comment).
func trackUsage(track chan<- string, idStr string) {
	if track == nil {
		return
	}
	select {
	case track <- idStr:
	default:
	}
}

const authCacheTTL = 5 * time.Minute

// authDBQueryTimeout bounds the DB fallback lookup in NewDBAuth (issue #238)
// — this runs on nearly every request, so it gets a tight deadline rather
// than the full request budget, matching handlers/status.go's convention for
// other hot/lightweight DB reads.
const authDBQueryTimeout = 2 * time.Second

// ParseKeyHashes parses a comma-separated list of HMAC-SHA256 hex digests
// (as stored in API_KEY_HASHES) into a set for lookup.
//
// It reads only its argument. Falling back to API_KEY here would make a parse
// function depend on the environment and would quietly widen the auth surface:
// API_KEY holds a plaintext key, not a digest, so such a value could never
// match anyway and would fail as a silent no-match rather than an error.
func ParseKeyHashes(raw string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, h := range strings.Split(raw, ",") {
		h = strings.TrimSpace(h)
		if h != "" {
			out[h] = struct{}{}
		}
	}
	return out
}

// ConstantTimeContains checks whether target matches any hash in validHashes in
// constant time using crypto/subtle.ConstantTimeCompare to avoid timing side-channel attacks.
func ConstantTimeContains(validHashes map[string]struct{}, target string) bool {
	var match int
	for hash := range validHashes {
		if len(hash) == len(target) {
			match |= subtle.ConstantTimeCompare([]byte(hash), []byte(target))
		}
	}
	return match == 1
}

// hmacKeyHash computes HMAC-SHA256 of key using API_KEY_SALT — used for the
// legacy API_KEY_HASHES env-var authentication path.
func hmacKeyHash(key string) string {
	salt := []byte(os.Getenv("API_KEY_SALT"))
	mac := hmac.New(sha256.New, salt)
	mac.Write([]byte(key))
	return hex.EncodeToString(mac.Sum(nil))
}

// hashKey is kept for backward compatibility with the TieredRateLimit
// middleware which references it by name.
func hashKey(key string) string {
	return hmacKeyHash(key)
}

// sha256KeyHash computes a plain SHA-256 hash of key — matches the hash stored
// in the api_keys table by handlers.sha256hex.
func sha256KeyHash(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

func authRedisCacheKey(hash string) string {
	return fmt.Sprintf("apiauth:%s", hash)
}

// withAuthenticatedKey attaches the authenticated key's id/network to ctx for
// downstream handlers (contextKeyAPIKeyID/contextKeyNetwork) and, when idStr
// parses as a UUID, also for the audit log writer (auditLogAPIKeyIDKey) so
// audit_log.api_key_id — and anything derived from it, like per-key usage
// rollups — is actually populated for DB-backed keys.
func withAuthenticatedKey(ctx context.Context, idStr, network string) context.Context {
	ctx = WithAPIKeyID(ctx, idStr)
	if network != "" {
		ctx = context.WithValue(ctx, contextKeyNetwork, network)
	}
	if id, err := uuid.Parse(idStr); err == nil {
		ctx = WithAuditAPIKeyID(ctx, &id)
	}
	// Also surfaces on StructuredLogging's end-of-request log line (issue
	// #239) — see requestLogState for why this can't just be another
	// context.WithValue.
	SetLogAPIKeyID(ctx, idStr)
	return ctx
}

// withLegacyAuthenticatedKey attaches identity, network and audit
// attribution to ctx for a request authenticated via the legacy
// API_KEY_HASHES env-var path (issue #616).
//
// Before this, a request on this path reached the handler with none of the
// above set: APIKeyIDFromContext returned "" (indistinguishable from "no
// auth ran"), NetworkFromContext silently fell through to its "testnet"
// default several layers downstream, and the audit_log row for the request
// had a NULL api_key_id with nothing else on it to say why — unattributable
// for billing, audit and incident response.
//
// This does not call withAuthenticatedKey: that helper assumes idStr may
// parse as a UUID naming a real api_keys row (WithAuditAPIKeyID requires
// one — audit_log.api_key_id has a foreign key to api_keys). A legacy
// env-var key has no such row, so fabricating a UUID for it would either
// violate that constraint or silently misattribute the request to an
// unrelated real key. Instead this sets LegacyEnvKeyID, a sentinel that is
// deliberately not a UUID, and records "legacy-env" as audit_log.auth_source
// (added by migration 0035) as the attribution in api_key_id's place.
func withLegacyAuthenticatedKey(ctx context.Context) context.Context {
	ctx = WithAPIKeyID(ctx, LegacyEnvKeyID)
	ctx = WithNetwork(ctx, LegacyEnvNetwork)
	ctx = WithAuditNetwork(ctx, LegacyEnvNetwork)
	ctx = WithAuditAuthSource(ctx, "legacy-env")
	SetLogAPIKeyID(ctx, LegacyEnvKeyID)
	return ctx
}

// NewDBAuth returns an authentication middleware that:
//  1. Looks up the hashed API key in Redis cache (5 min TTL).
//  2. Falls back to the api_keys database table (active keys only).
//  3. Falls back to legacy HMAC-SHA256 env-var authentication (API_KEY_HASHES).
//
// On success, api_key_id and network are attached to the request context.
// Unauthenticated requests receive 401 unless the path is excluded.
func NewDBAuth(cfg DBAuthConfig) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Public paths — skip auth entirely.
			path := r.URL.Path
			// /v1/version stays authenticated: it publishes the exact commit
			// SHA and applied schema version, which narrows an attacker's
			// search for known-vulnerable code paths. Operators debugging
			// "which build is live?" have a key; anonymous callers do not
			// need one. /v1/ready already covers unauthenticated liveness.
			if path == "/v1/health" || path == "/v1/ready" || path == "/metrics" {
				next.ServeHTTP(w, r)
				return
			}
			if !strings.HasPrefix(path, "/v1/") && path != "/ws" {
				next.ServeHTTP(w, r)
				return
			}

			key := r.Header.Get("X-API-Key")
			if key == "" {
				httputil.WriteErrorCtx(r.Context(), w, http.StatusUnauthorized, httputil.UNAUTHORIZED, "Unauthorized")
				return
			}

			// ── 1. Redis cache ──────────────────────────────────────────────
			dbHash := sha256KeyHash(key)
			if cfg.Redis != nil {
				if cached, err := cfg.Redis.Get(r.Context(), authRedisCacheKey(dbHash)).Result(); err == nil {
					// Cached value format: "<uuid>:<network>"
					parts := strings.SplitN(cached, ":", 2)
					network := ""
					if len(parts) == 2 {
						network = parts[1]
					}
					trackUsage(cfg.UsageTrack, parts[0])
					ctx := withAuthenticatedKey(r.Context(), parts[0], network)
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
			}

			// ── 2. Database lookup ──────────────────────────────────────────
			if cfg.DB != nil {
				dbCtx, cancel := context.WithTimeout(r.Context(), authDBQueryTimeout)
				defer cancel()

				var id, network string
				err := cfg.DB.QueryRow(dbCtx,
					`SELECT id, network FROM api_keys WHERE key_hash = $1 AND revoked_at IS NULL`,
					dbHash,
				).Scan(&id, &network)
				if err == nil {
					// Populate Redis cache so the next request is O(1).
					if cfg.Redis != nil {
						cfg.Redis.Set(r.Context(), authRedisCacheKey(dbHash),
							id+":"+network, authCacheTTL)
					}
					trackUsage(cfg.UsageTrack, id)
					ctx := withAuthenticatedKey(r.Context(), id, network)
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
			}

			// ── 3. Legacy env-var fallback (API_KEY_HASHES) ────────────────
			validHashes := ParseKeyHashes(os.Getenv("API_KEY_HASHES"))
			if len(validHashes) > 0 {
				if ConstantTimeContains(validHashes, hmacKeyHash(key)) {
					ctx := withLegacyAuthenticatedKey(r.Context())
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
			}

			httputil.WriteErrorCtx(r.Context(), w, http.StatusUnauthorized, httputil.UNAUTHORIZED, "Unauthorized")
		})
	}
}

// Auth validates X-API-Key for protected API and WebSocket routes.
// GET /v1/health remains public. validHashes is the pre-parsed set of
// accepted HMAC-SHA256 hex digests. When validHashes is empty all requests
// pass through (auth is disabled — suitable for local development).
func Auth(validHashes map[string]struct{}, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/health" {
			next.ServeHTTP(w, r)
			return
		}

		if len(validHashes) == 0 {
			next.ServeHTTP(w, r)
			return
		}

		if !strings.HasPrefix(r.URL.Path, "/v1/") && r.URL.Path != "/ws" {
			next.ServeHTTP(w, r)
			return
		}

		key := r.Header.Get("X-API-Key")
		if key == "" {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusUnauthorized, httputil.UNAUTHORIZED, "Unauthorized")
			return
		}

		if !ConstantTimeContains(validHashes, hmacKeyHash(key)) {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusUnauthorized, httputil.UNAUTHORIZED, "Unauthorized")
			return
		}

		next.ServeHTTP(w, r)
	})
}

// APIKey is a convenience wrapper around Auth that reads API_KEY_HASHES from
// the environment on each call.
func APIKey(next http.Handler) http.Handler {
	return Auth(ParseKeyHashes(os.Getenv("API_KEY_HASHES")), next)
}

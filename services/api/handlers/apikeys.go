package handlers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/Depo-dev/trident/services/api/cursor"
	"github.com/Depo-dev/trident/services/api/internal/httputil"
	"github.com/Depo-dev/trident/services/api/internal/metrics"
	"github.com/Depo-dev/trident/services/api/middleware"
	"github.com/Depo-dev/trident/services/api/validation"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// apiKeyListDefaultLimit/apiKeyListMaxLimit bound GET /v1/api-keys pagination
// (issue #220) — same shape as ListContracts/ListEvents: a bounded default
// page, and a hard ceiling so a client can never force one query to return
// an unbounded result set.
const (
	apiKeyListDefaultLimit = 50
	apiKeyListMaxLimit     = 200
)

// apiKeyQueryTimeout bounds the DB calls in the api-key admin handlers so a
// runaway query can't hold a pool connection for the request's full budget
// (issue #238).
const apiKeyQueryTimeout = 5 * time.Second

// APIKeyConfig wires the api-key handlers.
type APIKeyConfig struct {
	AdminKey string
	DB       *pgxpool.Pool
	// Redis is used for cache invalidation on key revocation.
	Redis *redis.Client
	// InvalidateTier evicts a key's cached rate-limit tier (by key hash) after
	// an admin tier change so the new limit applies promptly instead of after
	// the tier-cache TTL. Wired to middleware.TierCache.Invalidate; nil-safe.
	InvalidateTier func(keyHash string)
}

// APIKeyResponse is returned for list/create operations.
// The Key field is only populated on creation and is never returned again.
type APIKeyResponse struct {
	ID            string  `json:"id"`
	KeyPrefix     string  `json:"key_prefix"`
	Key           *string `json:"key,omitempty"`
	Label         string  `json:"label"`
	Network       string  `json:"network"`
	RateLimitTier string  `json:"rate_limit_tier"`
	CreatedBy     *string `json:"created_by,omitempty"`
	LastUsedAt    *string `json:"last_used_at"`
	RequestCount  int64   `json:"request_count"`
	RevokedAt     *string `json:"revoked_at,omitempty"`
	// ExpiresAt is when the key stops authenticating (issue #752); omitted
	// for a key that never expires.
	ExpiresAt *string `json:"expires_at,omitempty"`
	CreatedAt string  `json:"created_at"`
}

// ListAPIKeysResponse is the response envelope for GET /v1/api-keys (issue
// #220) — the same has_more/next_cursor shape as ListEventsResponse, so
// every keyset-paginated list endpoint in this API is walked the same way.
type ListAPIKeysResponse struct {
	APIKeys    []APIKeyResponse `json:"api_keys"`
	HasMore    bool             `json:"has_more"`
	NextCursor *string          `json:"next_cursor"`
}

type createKeyRequest struct {
	Label         string         `json:"label"`
	Network       string         `json:"network"`
	RateLimitTier string         `json:"rate_limit_tier"`
	CreatedBy     string         `json:"created_by"`
	ExpiresAt     optionalExpiry `json:"expires_at"`
}

type updateKeyRequest struct {
	Label         *string        `json:"label"`
	RateLimitTier *string        `json:"rate_limit_tier"`
	ExpiresAt     optionalExpiry `json:"expires_at"`
}

type rotateKeyRequest struct {
	ExpiresAt optionalExpiry `json:"expires_at"`
}

var errInvalidExpiry = errors.New("expires_at must be a future RFC 3339 timestamp or null")

// optionalExpiry is an expires_at request field that distinguishes "absent"
// (Set=false, leave unchanged) from an explicit null (Set=true, Value=nil,
// clear the expiry) from a timestamp (Set=true, Value non-nil). A timestamp
// must be in the future (issue #752).
type optionalExpiry struct {
	Set   bool
	Value *time.Time
}

func (o *optionalExpiry) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Value = nil
		return nil
	}
	var raw string
	if err := json.Unmarshal(b, &raw); err != nil {
		return errInvalidExpiry
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil || !t.After(time.Now()) {
		return errInvalidExpiry
	}
	t = t.UTC()
	o.Value = &t
	return nil
}

func formatExpiry(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

// writeBodyDecodeError reports a request-body decode failure with the right
// status and message.
func writeBodyDecodeError(w http.ResponseWriter, r *http.Request, err error) {
	if middleware.IsBodyTooLarge(err) {
		middleware.WriteBodyTooLarge(w, r)
		return
	}
	msg := "invalid JSON body"
	if errors.Is(err, errInvalidExpiry) {
		msg = errInvalidExpiry.Error()
	}
	httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, msg)
}

// requireAdmin checks admin key and DB availability, writing an appropriate
// error response and returning false when the handler should abort.
func requireAdmin(cfg APIKeyConfig, w http.ResponseWriter, r *http.Request) bool {
	if cfg.AdminKey == "" {
		httputil.WriteErrorCtx(r.Context(), w, http.StatusForbidden, httputil.UNAUTHORIZED, "admin API key is not configured")
		return false
	}
	if !validAdminKey(cfg.AdminKey, r.Header.Get("X-Admin-Key")) {
		httputil.WriteErrorCtx(r.Context(), w, http.StatusUnauthorized, httputil.UNAUTHORIZED, "invalid or missing admin key")
		return false
	}
	if cfg.DB == nil {
		httputil.WriteErrorCtx(r.Context(), w, http.StatusServiceUnavailable, httputil.UNAVAILABLE, "database unavailable")
		return false
	}
	return true
}

// CreateAPIKey handles POST /v1/api-keys (admin-only).
//
// Generates a key: "trident_" + 32 random hex bytes. Only the SHA-256 hash is
// stored. The plaintext key is returned exactly once in the response.
func CreateAPIKey(cfg APIKeyConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(cfg, w, r) {
			return
		}

		var req createKeyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeBodyDecodeError(w, r, err)
			return
		}
		network, verr := validation.ValidateNetwork("network", req.Network, "mainnet")
		if verr != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, verr.Message)
			return
		}
		req.Network = network
		if req.RateLimitTier == "" {
			req.RateLimitTier = "standard"
		}

		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "failed to generate key")
			return
		}
		plaintext := "trident_" + hex.EncodeToString(raw)
		hash := sha256hex(plaintext)
		prefix := plaintext[:16]

		var createdBy *string
		if req.CreatedBy != "" {
			createdBy = &req.CreatedBy
		}

		ctx, cancel := context.WithTimeout(r.Context(), apiKeyQueryTimeout)
		defer cancel()

		var id string
		var createdAt time.Time
		err := cfg.DB.QueryRow(ctx,
			`INSERT INTO api_keys (key_hash, key_prefix, label, network, rate_limit_tier, created_by, expires_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)
			 RETURNING id, created_at`,
			hash, prefix, req.Label, req.Network, req.RateLimitTier, createdBy, req.ExpiresAt.Value,
		).Scan(&id, &createdAt)
		if err != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "failed to create api key")
			return
		}

		ts := createdAt.UTC().Format(time.RFC3339)
		writeJSON(w, http.StatusCreated, APIKeyResponse{
			ID:            id,
			KeyPrefix:     prefix,
			Key:           &plaintext,
			Label:         req.Label,
			Network:       req.Network,
			RateLimitTier: req.RateLimitTier,
			CreatedBy:     createdBy,
			CreatedAt:     ts,
			ExpiresAt:     formatExpiry(req.ExpiresAt.Value),
		})
	}
}

// ListAPIKeys handles GET /v1/api-keys (admin-only).
//
// Returns all keys with key_prefix, last_used_at, and request_count.
// The full plaintext key and hash are never returned.
func ListAPIKeys(cfg APIKeyConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(cfg, w, r) {
			return
		}

		if verr := validation.RejectUnknownParams(r.URL.Query(), "limit", "cursor"); verr != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, verr.Message)
			return
		}

		limit := apiKeyListDefaultLimit
		if l := r.URL.Query().Get("limit"); l != "" {
			n, err := parseInt(l)
			if err != nil || n <= 0 || n > apiKeyListMaxLimit {
				httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT,
					fmt.Sprintf("limit must be an integer between 1 and %d", apiKeyListMaxLimit))
				return
			}
			limit = n
		}

		// created_at is not unique on its own (two keys can be created in the
		// same instant), so the keyset is (created_at, id) — a total order
		// with id as tiebreaker — not created_at alone (issue #220).
		//
		// Both bind as untyped nil (SQL NULL) when no cursor is supplied,
		// never as a zero time / empty string: binding "" for a ::uuid
		// parameter fails to cast regardless of the WHERE clause's IS NULL
		// branch, because the cast happens while binding the parameter
		// value, before the OR can short-circuit it away.
		var cursorCreatedAt, cursorID any
		if c := r.URL.Query().Get("cursor"); c != "" {
			t, id, err := cursor.DecodeKeyset(c)
			if err != nil {
				httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, "cursor is not a valid pagination cursor")
				return
			}
			cursorCreatedAt, cursorID = t, id
		}

		ctx, cancel := context.WithTimeout(r.Context(), apiKeyQueryTimeout)
		defer cancel()

		// LIMIT $3 fetches one extra row past the page: its presence is how
		// has_more is known without a separate COUNT query.
		rows, err := cfg.DB.Query(ctx,
			`SELECT id, key_prefix, label, network, rate_limit_tier, created_by,
			        last_used_at, request_count, revoked_at, created_at, expires_at
			 FROM api_keys
			 WHERE ($1::timestamptz IS NULL OR (created_at, id) < ($1::timestamptz, $2::uuid))
			 ORDER BY created_at DESC, id DESC
			 LIMIT $3`,
			cursorCreatedAt, cursorID, limit+1,
		)
		if err != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "failed to list api keys")
			return
		}
		defer rows.Close()

		type row struct {
			key       APIKeyResponse
			createdAt time.Time
		}
		fetched := make([]row, 0, limit+1)
		for rows.Next() {
			var k APIKeyResponse
			var lastUsedAt, revokedAt, expiresAt *time.Time
			var createdAt time.Time
			if err := rows.Scan(&k.ID, &k.KeyPrefix, &k.Label, &k.Network,
				&k.RateLimitTier, &k.CreatedBy, &lastUsedAt, &k.RequestCount,
				&revokedAt, &createdAt, &expiresAt); err != nil {
				httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "scan error")
				return
			}
			k.CreatedAt = createdAt.UTC().Format(time.RFC3339)
			k.ExpiresAt = formatExpiry(expiresAt)
			if lastUsedAt != nil {
				s := lastUsedAt.UTC().Format(time.RFC3339)
				k.LastUsedAt = &s
			}
			if revokedAt != nil {
				s := revokedAt.UTC().Format(time.RFC3339)
				k.RevokedAt = &s
			}
			fetched = append(fetched, row{key: k, createdAt: createdAt})
		}
		if rows.Err() != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "query error")
			return
		}

		hasMore := len(fetched) > limit
		if hasMore {
			fetched = fetched[:limit]
		}

		keys := make([]APIKeyResponse, len(fetched))
		for i, f := range fetched {
			keys[i] = f.key
		}

		var nextCursor *string
		if hasMore {
			last := fetched[len(fetched)-1]
			c := cursor.EncodeKeyset(last.createdAt, last.key.ID)
			nextCursor = &c
		}

		writeJSON(w, http.StatusOK, ListAPIKeysResponse{
			APIKeys:    keys,
			HasMore:    hasMore,
			NextCursor: nextCursor,
		})
	}
}

// UpdateAPIKey handles PATCH /v1/api-keys/{id} (admin-only).
//
// Allows updating the label, rate_limit_tier or expires_at of an active key.
// expires_at: null clears the expiry; omitting it leaves it unchanged.
func UpdateAPIKey(cfg APIKeyConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(cfg, w, r) {
			return
		}

		// Path ids go through the shared UUID validator so a malformed id is a
		// clear INVALID_ARGUMENT instead of a database-shaped failure (#222).
		id := r.PathValue("id")
		if verr := validation.ValidateUUID("id", id); verr != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, verr.Message)
			return
		}

		var req updateKeyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeBodyDecodeError(w, r, err)
			return
		}
		if req.Label == nil && req.RateLimitTier == nil && !req.ExpiresAt.Set {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, "at least one of label, rate_limit_tier or expires_at is required")
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), apiKeyQueryTimeout)
		defer cancel()

		var k APIKeyResponse
		var lastUsedAt, expiresAt *time.Time
		var createdAt time.Time
		var keyHash string
		err := cfg.DB.QueryRow(ctx,
			`UPDATE api_keys
			 SET label           = COALESCE($2, label),
			     rate_limit_tier = COALESCE($3, rate_limit_tier),
			     expires_at      = CASE WHEN $4::boolean THEN $5::timestamptz ELSE expires_at END
			 WHERE id = $1 AND revoked_at IS NULL
			 RETURNING id, key_prefix, label, network, rate_limit_tier,
			           last_used_at, request_count, created_at, key_hash, expires_at`,
			id, req.Label, req.RateLimitTier, req.ExpiresAt.Set, req.ExpiresAt.Value,
		).Scan(&k.ID, &k.KeyPrefix, &k.Label, &k.Network, &k.RateLimitTier,
			&lastUsedAt, &k.RequestCount, &createdAt, &keyHash, &expiresAt)
		if err == pgx.ErrNoRows {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusNotFound, httputil.NOT_FOUND, "api key not found")
			return
		}
		if err != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "failed to update api key")
			return
		}

		// A tier change must evict the cached tier so the new limit applies on
		// the next request rather than after the tier-cache TTL (issue #229).
		if req.RateLimitTier != nil && cfg.InvalidateTier != nil {
			cfg.InvalidateTier(keyHash)
		}

		// An expiry change must evict the cached auth entry: it was cached
		// with a TTL derived from the old expires_at (issue #752).
		if req.ExpiresAt.Set && cfg.Redis != nil {
			cfg.Redis.Del(r.Context(), fmt.Sprintf("apiauth:%s", keyHash))
		}

		k.CreatedAt = createdAt.UTC().Format(time.RFC3339)
		k.ExpiresAt = formatExpiry(expiresAt)
		if lastUsedAt != nil {
			s := lastUsedAt.UTC().Format(time.RFC3339)
			k.LastUsedAt = &s
		}
		writeJSON(w, http.StatusOK, k)
	}
}

// RotateAPIKey handles POST /v1/api-keys/{id}/rotate (admin-only).
//
// Creates a new replacement key inheriting the network, rate_limit_tier, and label
// of the existing active key, allowing seamless zero-downtime key rotation with an overlap window.
// The old key remains active until explicitly revoked.
func RotateAPIKey(cfg APIKeyConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(cfg, w, r) {
			return
		}

		id := r.PathValue("id")
		if verr := validation.ValidateUUID("id", id); verr != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, verr.Message)
			return
		}

		// The body is optional: it only carries expires_at for the new key.
		var req rotateKeyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeBodyDecodeError(w, r, err)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), apiKeyQueryTimeout)
		defer cancel()

		var oldLabel, oldNetwork, oldTier string
		var oldCreatedBy *string
		err := cfg.DB.QueryRow(ctx,
			`SELECT label, network, rate_limit_tier, created_by
			 FROM api_keys
			 WHERE id = $1 AND revoked_at IS NULL`,
			id,
		).Scan(&oldLabel, &oldNetwork, &oldTier, &oldCreatedBy)
		if err == pgx.ErrNoRows {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusNotFound, httputil.NOT_FOUND, "active api key not found for rotation")
			return
		}
		if err != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "failed to query api key")
			return
		}

		// Generate new key: "trident_" + 32 random hex bytes
		rawBytes := make([]byte, 32)
		if _, err := rand.Read(rawBytes); err != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "failed to generate key entropy")
			return
		}
		plainKey := "trident_" + hex.EncodeToString(rawBytes)
		keyHash := sha256hex(plainKey)
		keyPrefix := plainKey[:16]

		newLabel := fmt.Sprintf("%s (rotated %s)", oldLabel, time.Now().UTC().Format("2006-01-02"))

		var resp APIKeyResponse
		var createdAt time.Time
		err = cfg.DB.QueryRow(ctx,
			`INSERT INTO api_keys (key_hash, key_prefix, label, network, rate_limit_tier, created_by, expires_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)
			 RETURNING id, key_prefix, label, network, rate_limit_tier, created_by, request_count, created_at`,
			keyHash, keyPrefix, newLabel, oldNetwork, oldTier, oldCreatedBy, req.ExpiresAt.Value,
		).Scan(&resp.ID, &resp.KeyPrefix, &resp.Label, &resp.Network, &resp.RateLimitTier, &resp.CreatedBy, &resp.RequestCount, &createdAt)
		if err != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "failed to store rotated api key")
			return
		}

		resp.Key = &plainKey
		resp.CreatedAt = createdAt.UTC().Format(time.RFC3339)
		resp.ExpiresAt = formatExpiry(req.ExpiresAt.Value)
		writeJSON(w, http.StatusCreated, resp)
	}
}

// DeleteAPIKey handles DELETE /v1/api-keys/{id} (admin-only).
//
// Soft-deletes the key by setting revoked_at. The key is immediately removed
// from the Redis auth cache so revocation takes effect on the next request
// without waiting for TTL expiry.
func DeleteAPIKey(cfg APIKeyConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireAdmin(cfg, w, r) {
			return
		}

		// Path ids go through the shared UUID validator so a malformed id is a
		// clear INVALID_ARGUMENT instead of a database-shaped failure (#222).
		id := r.PathValue("id")
		if verr := validation.ValidateUUID("id", id); verr != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, verr.Message)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), apiKeyQueryTimeout)
		defer cancel()

		var keyHash string
		err := cfg.DB.QueryRow(ctx,
			`UPDATE api_keys
			 SET revoked_at = NOW()
			 WHERE id = $1 AND revoked_at IS NULL
			 RETURNING key_hash`,
			id,
		).Scan(&keyHash)
		if err == pgx.ErrNoRows {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusNotFound, httputil.NOT_FOUND, "api key not found")
			return
		}
		if err != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "failed to revoke api key")
			return
		}

		// Immediately evict the revoked key from Redis so auth rejects it
		// on the next request rather than waiting for the 5-minute TTL.
		if cfg.Redis != nil {
			cfg.Redis.Del(r.Context(), fmt.Sprintf("apiauth:%s", keyHash))
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

func sha256hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h)
}

// usageExecer is the slice of *pgxpool.Pool the usage tracker needs.
type usageExecer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// NewAPIKeyUsageTracker returns a channel-based background aggregator for
// issue #139. The caller should send a key UUID on the channel after every
// successful auth. The aggregator batches pending updates and flushes them to
// postgres every flushInterval (typically 5s). Call stop() on shutdown to
// drain the channel before exit.
//
// A failed flush keeps the pending counts and retries them on the next tick
// (issue #753); each failure is logged and counted.
func NewAPIKeyUsageTracker(db *pgxpool.Pool, flushInterval time.Duration) (track chan<- string, stop func()) {
	return newUsageTracker(db, flushInterval)
}

func newUsageTracker(db usageExecer, flushInterval time.Duration) (track chan<- string, stop func()) {
	ch := make(chan string, 4096)

	go func() {
		ticker := time.NewTicker(flushInterval)
		defer ticker.Stop()

		pending := map[string]int64{}

		flush := func() {
			if len(pending) == 0 {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ids := make([]string, 0, len(pending))
			counts := make([]int64, 0, len(pending))
			for id, count := range pending {
				ids = append(ids, id)
				counts = append(counts, count)
			}
			if _, err := db.Exec(ctx,
				`UPDATE api_keys AS k
				 SET request_count = k.request_count + u.count,
				     last_used_at  = NOW()
				 FROM (SELECT unnest($1::uuid[]) AS id, unnest($2::bigint[]) AS count) AS u
				 WHERE k.id = u.id`,
				ids, counts,
			); err != nil {
				metrics.APIKeyUsageFlushFailuresTotal.Inc()
				slog.Error("api key usage flush failed; will retry", "keys", len(ids), "error", err)
				return
			}
			pending = map[string]int64{}
		}

		for {
			select {
			case id, ok := <-ch:
				if !ok {
					flush()
					return
				}
				pending[id]++
			case <-ticker.C:
				flush()
			}
		}
	}()

	return ch, func() { close(ch) }
}

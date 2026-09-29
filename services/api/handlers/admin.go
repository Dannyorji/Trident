package handlers

import (
	"context"
	"crypto/subtle"
	"net/http"
	"time"

	"github.com/Depo-dev/trident/services/api/internal/httputil"
	"github.com/Depo-dev/trident/services/api/validation"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// adminStatsTimeout bounds how long the admin endpoint waits on PgBouncer.
const adminStatsTimeout = 5 * time.Second

// DBStats is the PgBouncer pooler snapshot returned by GET /v1/admin/db.
//
// Pools and Stats hold the raw rows from `SHOW POOLS` and `SHOW STATS`, each as
// an ordered list of column-name to value maps. Keeping the rows verbatim means
// the response stays faithful to whatever columns the running PgBouncer version
// reports, without this code having to track schema changes between versions.
type DBStats struct {
	Pools []map[string]any `json:"pools"`
	Stats []map[string]any `json:"stats"`
}

// AdminConfig wires up the admin DB endpoint.
//
// AdminKey is the shared secret the caller must present in the X-Admin-Key
// header. StatsFunc fetches a live PgBouncer snapshot. If AdminKey is empty or
// StatsFunc is nil the endpoint is considered disabled and returns 503, so an
// operator can leave it off simply by not setting ADMIN_API_KEY.
type AdminConfig struct {
	AdminKey  string
	StatsFunc func(ctx context.Context) (*DBStats, error)
	DB        *pgxpool.Pool // for audit log queries
}

// AdminDB handles GET /v1/admin/db.
//
// It returns PgBouncer pool utilisation and cumulative stats (SHOW POOLS /
// SHOW STATS) for capacity planning (issue #87). The caller must present a
// valid admin key in the X-Admin-Key header.
func AdminDB(cfg AdminConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cfg.AdminKey == "" || cfg.StatsFunc == nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusServiceUnavailable, httputil.UNAVAILABLE, "admin DB endpoint is not configured")
			return
		}

		if !validAdminKey(cfg.AdminKey, r.Header.Get("X-Admin-Key")) {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusUnauthorized, httputil.UNAUTHORIZED, "invalid or missing admin key")
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), adminStatsTimeout)
		defer cancel()

		stats, err := cfg.StatsFunc(ctx)
		if err != nil {
			// The PgBouncer admin console is the upstream here, so a failure to
			// reach it is a bad-gateway condition rather than our own error.
			httputil.WriteErrorCtx(r.Context(), w, http.StatusBadGateway, httputil.UNAVAILABLE, "could not read PgBouncer stats")
			return
		}

		writeJSON(w, http.StatusOK, stats)
	}
}

// validAdminKey reports whether provided matches expected, using a constant-time
// comparison so the endpoint does not leak the key length or content via timing.
func validAdminKey(expected, provided string) bool {
	if provided == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(provided)) == 1
}

// AdminKeyUsageResponse is the response for GET /v1/admin/keys/:id/usage.
type AdminKeyUsageResponse struct {
	APIKeyID           string               `json:"api_key_id"`
	From               string               `json:"from"`
	To                 string               `json:"to"`
	TotalRequests      int64                `json:"total_requests"`
	SuccessfulRequests int64                `json:"successful_requests"`
	ByEndpoint         []AdminEndpointUsage `json:"by_endpoint"`
}

type AdminEndpointUsage struct {
	Endpoint      string  `json:"endpoint"`
	Requests      int64   `json:"requests"`
	AvgDurationMs float64 `json:"avg_duration_ms"`
}

// AdminKeyUsage handles GET /v1/admin/keys/:id/usage.
//
// Returns aggregated usage statistics for an API key from the audit log.
// Query params: from (RFC3339, required), to (RFC3339, required).
func AdminKeyUsage(cfg AdminConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cfg.AdminKey == "" || cfg.DB == nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusServiceUnavailable, httputil.UNAVAILABLE, "admin usage endpoint is not configured")
			return
		}

		if !validAdminKey(cfg.AdminKey, r.Header.Get("X-Admin-Key")) {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusUnauthorized, httputil.UNAUTHORIZED, "invalid or missing admin key")
			return
		}

		q := r.URL.Query()
		if verr := validation.RejectUnknownParams(q, "from", "to"); verr != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, verr.Message)
			return
		}

		keyIDStr := r.PathValue("id")
		if verr := validation.ValidateUUID("id", keyIDStr); verr != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, verr.Message)
			return
		}
		keyID, err := uuid.Parse(keyIDStr)
		if err != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, "id must be a valid UUID v4")
			return
		}

		from, to, verr := validation.ValidateTimeRange("from", "to", q.Get("from"), q.Get("to"), 31*24*time.Hour)
		if verr != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, verr.Message)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), adminStatsTimeout)
		defer cancel()

		// Total requests
		var totalReqs int64
		err = cfg.DB.QueryRow(ctx,
			`SELECT COUNT(*) FROM audit_log WHERE api_key_id = $1 AND ts >= $2 AND ts < $3`,
			keyID, from, to,
		).Scan(&totalReqs)
		if err != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "failed to query total requests")
			return
		}

		// Successful requests (2xx)
		var successReqs int64
		err = cfg.DB.QueryRow(ctx,
			`SELECT COUNT(*) FROM audit_log WHERE api_key_id = $1 AND ts >= $2 AND ts < $3 AND status_code < 400`,
			keyID, from, to,
		).Scan(&successReqs)
		if err != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "failed to query successful requests")
			return
		}

		// By endpoint
		rows, err := cfg.DB.Query(ctx,
			`SELECT endpoint, COUNT(*) as req_count, AVG(duration_ms)::float8 as avg_duration
			 FROM audit_log
			 WHERE api_key_id = $1 AND ts >= $2 AND ts < $3
			 GROUP BY endpoint
			 ORDER BY req_count DESC`,
			keyID, from, to,
		)
		if err != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "failed to query endpoint breakdown")
			return
		}
		defer rows.Close()

		// Initialized non-nil so an empty window serializes as [] rather than
		// null — the generated SDK models (Rust Vec, Python list) reject null
		// for this field, and "no requests" is an empty breakdown, not an
		// absent one (issue #513).
		byEndpoint := []AdminEndpointUsage{}
		for rows.Next() {
			var eu AdminEndpointUsage
			if err := rows.Scan(&eu.Endpoint, &eu.Requests, &eu.AvgDurationMs); err != nil {
				httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "scan error")
				return
			}
			byEndpoint = append(byEndpoint, eu)
		}

		resp := AdminKeyUsageResponse{
			APIKeyID:           keyIDStr,
			From:               from.UTC().Format(time.RFC3339),
			To:                 to.UTC().Format(time.RFC3339),
			TotalRequests:      totalReqs,
			SuccessfulRequests: successReqs,
			ByEndpoint:         byEndpoint,
		}

		writeJSON(w, http.StatusOK, resp)
	}
}

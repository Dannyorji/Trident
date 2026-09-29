package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/Depo-dev/trident/services/api/internal/httputil"
	"github.com/Depo-dev/trident/services/api/middleware"
	"github.com/Depo-dev/trident/services/api/validation"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// defaultUsageLookback bounds the self-serve usage window when the caller
// omits from/to query parameters.
const defaultUsageLookback = 30 * 24 * time.Hour

// UsageConfig wires the self-serve usage handler.
type UsageConfig struct {
	DB *pgxpool.Pool
}

// UsageRollupRow is one daily bucket of an API key's usage.
type UsageRollupRow struct {
	PeriodStart   string  `json:"period_start"`
	PeriodEnd     string  `json:"period_end"`
	RequestCount  int64   `json:"request_count"`
	ErrorCount    int64   `json:"error_count"`
	AvgDurationMs float64 `json:"avg_duration_ms"`
}

// UsageResponse is returned by both the self-serve and admin usage-rollup
// endpoints.
type UsageResponse struct {
	APIKeyID      string           `json:"api_key_id"`
	From          string           `json:"from"`
	To            string           `json:"to"`
	TotalRequests int64            `json:"total_requests"`
	TotalErrors   int64            `json:"total_errors"`
	Days          []UsageRollupRow `json:"days"`
}

// RollupUsage (re-)aggregates audit_log into usage_rollup for every UTC day
// bucket touched since `since`. It is an upsert: safe to call repeatedly (and
// with overlapping windows) from a periodic job, since each call recomputes —
// rather than incrementally adds to — the totals for the buckets it touches.
//
// Calling with `since` a day or two in the past re-covers the current day
// (which fills in as the day progresses) and catches any audit_log rows that
// arrived late relative to the previous run, since the audit writer batches
// asynchronously.

func RollupUsage(ctx context.Context, db *pgxpool.Pool, since time.Time) error {
	_, err := db.Exec(ctx, `
		INSERT INTO usage_rollup (api_key_id, period_start, period_end, request_count, error_count, avg_duration_ms, updated_at)
		SELECT
			api_key_id,
			date_trunc('day', ts) AS period_start,
			date_trunc('day', ts) + INTERVAL '1 day' AS period_end,
			COUNT(*) AS request_count,
			COUNT(*) FILTER (WHERE status_code >= 400) AS error_count,
			AVG(duration_ms)::float8 AS avg_duration_ms,
			NOW() AS updated_at
		FROM audit_log
		WHERE api_key_id IS NOT NULL AND ts >= $1
		GROUP BY api_key_id, date_trunc('day', ts)
		ON CONFLICT (api_key_id, period_start) DO UPDATE SET
			period_end      = EXCLUDED.period_end,
			request_count   = EXCLUDED.request_count,
			error_count     = EXCLUDED.error_count,
			avg_duration_ms = EXCLUDED.avg_duration_ms,
			updated_at      = NOW()
	`, since)
	return err
}

// RunUsageRollupLoop periodically re-aggregates the last `lookback` of
// audit_log into usage_rollup. Called from main() as a background goroutine;
// stops when ctx is cancelled.
//
// Rollup lag: a usage_rollup row can lag the raw audit_log by at most
// `interval` (default 5m) plus the audit writer's own flush interval
// (500ms) — the two together bound how stale a usage figure can be.
func RunUsageRollupLoop(ctx context.Context, db *pgxpool.Pool, interval, lookback time.Duration) {
	run := func() {
		rollupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		_ = RollupUsage(rollupCtx, db, time.Now().Add(-lookback))
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	run()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

// queryUsageRollup reads usage_rollup rows for a key over [from, to) and
// returns them alongside the summed totals.
func queryUsageRollup(ctx context.Context, db *pgxpool.Pool, keyID uuid.UUID, from, to time.Time) (UsageResponse, error) {
	resp := UsageResponse{
		APIKeyID: keyID.String(),
		From:     from.UTC().Format(time.RFC3339),
		To:       to.UTC().Format(time.RFC3339),
		Days:     []UsageRollupRow{},
	}

	rows, err := db.Query(ctx,
		`SELECT period_start, period_end, request_count, error_count, avg_duration_ms
		 FROM usage_rollup
		 WHERE api_key_id = $1 AND period_start >= $2 AND period_start < $3
		 ORDER BY period_start ASC`,
		keyID, from, to,
	)
	if err != nil {
		return resp, err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			periodStart, periodEnd time.Time
			row                    UsageRollupRow
		)
		if err := rows.Scan(&periodStart, &periodEnd, &row.RequestCount, &row.ErrorCount, &row.AvgDurationMs); err != nil {
			return resp, err
		}
		row.PeriodStart = periodStart.UTC().Format(time.RFC3339)
		row.PeriodEnd = periodEnd.UTC().Format(time.RFC3339)
		resp.TotalRequests += row.RequestCount
		resp.TotalErrors += row.ErrorCount
		resp.Days = append(resp.Days, row)
	}
	if err := rows.Err(); err != nil {
		return resp, err
	}

	return resp, nil
}

// parseUsageWindow parses optional from/to RFC3339 query parameters, defaulting
// to the last defaultUsageLookback when omitted.
func parseUsageWindow(r *http.Request) (from, to time.Time, err error) {
	to = time.Now().UTC()
	if v := r.URL.Query().Get("to"); v != "" {
		to, err = time.Parse(time.RFC3339, v)
		if err != nil {
			return from, to, err
		}
	}

	from = to.Add(-defaultUsageLookback)
	if v := r.URL.Query().Get("from"); v != "" {
		from, err = time.Parse(time.RFC3339, v)
		if err != nil {
			return from, to, err
		}
	}
	return from, to, nil
}

// KeyUsage handles GET /v1/usage (self-serve).
//
// Returns the authenticated caller's own usage rollup for an optional
// [from, to) window (RFC3339 query params; defaults to the last 30 days).
// Requires a DB-backed API key — legacy env-hash auth has no key id to key
// the rollup on.
func KeyUsage(cfg UsageConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cfg.DB == nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusServiceUnavailable, httputil.UNAVAILABLE, "usage endpoint is not configured")
			return
		}

		if verr := validation.RejectUnknownParams(r.URL.Query(), "from", "to"); verr != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, verr.Message)
			return
		}

		idStr := middleware.APIKeyIDFromContext(r.Context())
		if idStr == "" {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusNotImplemented, httputil.UNAVAILABLE, "usage metering requires a DB-backed API key")
			return
		}
		// Legacy env-var keys (issue #616) authenticate successfully and
		// reach here with idStr == middleware.LegacyEnvKeyID, a deliberately
		// non-UUID sentinel — they have no api_keys row to key a rollup on,
		// so this can never become a UUID no matter how APIKeyIDFromContext
		// changes. Checked ahead of uuid.Parse so that case gets its own
		// clear, correct 501 instead of falling into "invalid authenticated
		// key id" (500), which would misreport a legacy key's absence of
		// per-key usage data as a server bug.
		if idStr == middleware.LegacyEnvKeyID {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusNotImplemented, httputil.UNAVAILABLE, "usage metering is not available for legacy env-var authenticated keys")
			return
		}
		keyID, err := uuid.Parse(idStr)
		if err != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "invalid authenticated key id")
			return
		}

		from, to, err := parseUsageWindow(r)
		if err != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, "invalid from/to timestamp, use RFC3339")
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), adminStatsTimeout)
		defer cancel()

		resp, err := queryUsageRollup(ctx, cfg.DB, keyID, from, to)
		if err != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "failed to query usage")
			return
		}

		writeJSON(w, http.StatusOK, resp)
	}
}

// AdminKeyUsageRollup handles GET /v1/admin/keys/{id}/usage-rollup (admin-only).
//
// Same shape as KeyUsage but for any key id, gated by X-Admin-Key. Backed by
// usage_rollup rather than a live audit_log scan, so it stays cheap regardless
// of how far back `from` reaches.
func AdminKeyUsageRollup(cfg AdminConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cfg.AdminKey == "" || cfg.DB == nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusServiceUnavailable, httputil.UNAVAILABLE, "admin usage endpoint is not configured")
			return
		}
		if !validAdminKey(cfg.AdminKey, r.Header.Get("X-Admin-Key")) {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusUnauthorized, httputil.UNAUTHORIZED, "invalid or missing admin key")
			return
		}

		if verr := validation.RejectUnknownParams(r.URL.Query(), "from", "to"); verr != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, verr.Message)
			return
		}

		keyIDStr := r.PathValue("id")
		keyID, err := uuid.Parse(keyIDStr)
		if err != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, "invalid api key id")
			return
		}

		from, to, err := parseUsageWindow(r)
		if err != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, "invalid from/to timestamp, use RFC3339")
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), adminStatsTimeout)
		defer cancel()

		resp, err := queryUsageRollup(ctx, cfg.DB, keyID, from, to)
		if err != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "failed to query usage")
			return
		}

		writeJSON(w, http.StatusOK, resp)
	}
}

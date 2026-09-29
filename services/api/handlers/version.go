package handlers

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// Build-time variables injected via -ldflags.
// These are set during the Docker build process and CI/CD pipeline.
var (
	// Version is the semantic version tag (e.g., "v1.2.3")
	Version = "dev"
	// CommitSHA is the full git commit hash
	CommitSHA = "unknown"
	// BuildTime is the RFC3339 timestamp when the binary was built
	BuildTime = "unknown"
)

// VersionResponse is the JSON body for GET /v1/version (issue #397).
type VersionResponse struct {
	Version        string  `json:"version"`
	CommitSHA      string  `json:"commit_sha"`
	BuildTimestamp string  `json:"build_timestamp"`
	SchemaVersion  *string `json:"schema_version"`
}

// Version handles GET /v1/version — exposes build metadata (issue #397).
//
// Returns the semantic version tag, git commit SHA, build timestamp, and the
// highest applied database migration version. This allows operators to
// quickly identify which build is running during incidents without digging
// through GHCR tags or pod specs.
//
// When the database is unavailable, schema_version is null but the endpoint
// still returns 200 with build metadata — the version endpoint must remain
// available even when dependencies are down.
func VersionHandler(db DBPool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp := VersionResponse{
			Version:        Version,
			CommitSHA:      CommitSHA,
			BuildTimestamp: BuildTime,
		}

		// Fetch the highest applied migration version from the database.
		// This is optional — if the database is unavailable, we still return
		// build metadata with schema_version: null.
		if db != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			defer cancel()

			// sqlx creates _sqlx_migrations with `version BIGINT`, so this
			// must scan into an integer — scanning into a string fails on
			// every healthy database and silently reports a null version.
			var schemaVersion int64
			row := db.QueryRow(ctx, `SELECT version FROM _sqlx_migrations ORDER BY version DESC LIMIT 1`)
			switch err := row.Scan(&schemaVersion); {
			case err == nil:
				v := strconv.FormatInt(schemaVersion, 10)
				resp.SchemaVersion = &v
			case errors.Is(err, pgx.ErrNoRows):
				// No migrations applied yet — a null version is accurate.
			default:
				// Build metadata is still useful when the database is
				// unreachable, so report it with a null version rather than
				// failing the request.
				slog.WarnContext(ctx, "version: could not read schema version", "err", err)
			}
		}

		writeJSON(w, http.StatusOK, resp)
	}
}

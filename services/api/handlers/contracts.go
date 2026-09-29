package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/Depo-dev/trident/services/api/cursor"
	"github.com/Depo-dev/trident/services/api/internal/httputil"
	"github.com/Depo-dev/trident/services/api/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ContractConfig wires up the admin contract CRUD endpoints.
type ContractConfig struct {
	AdminKey string
	DB       *pgxpool.Pool
}

// ContractResponse is the JSON representation of an indexed_contracts row.
type ContractResponse struct {
	ID         string  `json:"id"`
	ContractID string  `json:"contract_id"`
	Network    *string `json:"network,omitempty"`
	Label      *string `json:"label,omitempty"`
	IndexFrom  int64   `json:"index_from"`
	CreatedAt  string  `json:"created_at"`
}

// CreateContractRequest is the JSON body for POST /v1/admin/contracts.
type CreateContractRequest struct {
	ContractID string `json:"contract_id"`
	Network    string `json:"network,omitempty"`
	Label      string `json:"label,omitempty"`
	IndexFrom  int64  `json:"index_from,omitempty"`
}

// ListContractsResponse is the response for GET /v1/admin/contracts — the
// same has_more/next_cursor envelope as ListEventsResponse and
// ListAPIKeysResponse (issue #220).
//
// next_cursor deliberately carries no `omitempty` (issue #575): on the last
// page it serialises as an explicit `null` rather than disappearing from the
// object. Absence and null are different signals to a strictly-typed SDK —
// an absent field cannot be told apart from a server too old to send it,
// whereas null unambiguously means "no next page". Every list endpoint in
// this API uses the explicit-null form, so the SDKs' auto-pagers need no
// per-endpoint special-casing.
type ListContractsResponse struct {
	Contracts  []ContractResponse `json:"contracts"`
	HasMore    bool               `json:"has_more"`
	NextCursor *string            `json:"next_cursor"`
}

// CreateContract handles POST /v1/admin/contracts.
// Admin-auth required. Registers a new contract for indexing.
func CreateContract(cfg ContractConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cfg.AdminKey == "" || cfg.DB == nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusServiceUnavailable, httputil.UNAVAILABLE, "admin contracts endpoint is not configured")
			return
		}

		if !validAdminKey(cfg.AdminKey, r.Header.Get("X-Admin-Key")) {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusUnauthorized, httputil.UNAUTHORIZED, "invalid or missing admin key")
			return
		}

		var req CreateContractRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			if middleware.IsBodyTooLarge(err) {
				middleware.WriteBodyTooLarge(w, r)
				return
			}
			httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, "invalid request body")
			return
		}

		if req.ContractID == "" {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, "contract_id is required")
			return
		}

		// Validate strkey format: must start with C and be 56 chars.
		if len(req.ContractID) != 56 || req.ContractID[0] != 'C' {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, "contract_id must be a valid 56-character strkey starting with C")
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		var id string
		var createdAt time.Time
		err := cfg.DB.QueryRow(ctx,
			`INSERT INTO indexed_contracts (contract_id, network, label, index_from)
			 VALUES ($1, NULLIF($2, ''), NULLIF($3, ''), $4)
			 ON CONFLICT (contract_id, network) DO UPDATE SET
			   label = EXCLUDED.label,
			   index_from = EXCLUDED.index_from
			 RETURNING id, created_at`,
			req.ContractID, req.Network, req.Label, req.IndexFrom,
		).Scan(&id, &createdAt)

		if err != nil {
			slog.ErrorContext(r.Context(), "failed to create contract", "err", err)
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "failed to register contract")
			return
		}

		writeJSON(w, http.StatusCreated, ContractResponse{
			ID:         id,
			ContractID: req.ContractID,
			Network:    strPtrOrNil(req.Network),
			Label:      strPtrOrNil(req.Label),
			IndexFrom:  req.IndexFrom,
			CreatedAt:  createdAt.Format(time.RFC3339),
		})
	}
}

// ListContracts handles GET /v1/admin/contracts.
// Admin-auth required. Returns a keyset-paginated list of registered contracts.
func ListContracts(cfg ContractConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cfg.AdminKey == "" || cfg.DB == nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusServiceUnavailable, httputil.UNAVAILABLE, "admin contracts endpoint is not configured")
			return
		}

		if !validAdminKey(cfg.AdminKey, r.Header.Get("X-Admin-Key")) {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusUnauthorized, httputil.UNAUTHORIZED, "invalid or missing admin key")
			return
		}

		limit := 100
		if l := r.URL.Query().Get("limit"); l != "" {
			if n, err := parseInt(l); err == nil && n > 0 && n <= 200 {
				limit = n
			}
		}

		// Opaque cursor (issue #220): id alone is the keyset (indexed_contracts.id
		// is a UUID primary key, already unique — no composite tiebreaker
		// needed, unlike the created_at-ordered listings). Previously this
		// bound the raw cursor query param directly as the id, so a client
		// could construct or tamper with a cursor by hand.
		var cursorID *string
		if raw := r.URL.Query().Get("cursor"); raw != "" {
			decoded, err := cursor.Decode(raw)
			if err != nil {
				httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, "cursor is not a valid pagination cursor")
				return
			}
			cursorID = &decoded
		}

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		query := `SELECT id, contract_id, network, label, index_from, created_at
				  FROM indexed_contracts
				  WHERE ($1::uuid IS NULL OR id > $1::uuid)
				  ORDER BY id ASC
				  LIMIT $2`

		rows, err := cfg.DB.Query(ctx, query, cursorID, limit+1)
		if err != nil {
			slog.ErrorContext(r.Context(), "failed to list contracts", "err", err)
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "failed to list contracts")
			return
		}
		defer rows.Close()

		contracts := make([]ContractResponse, 0, limit)
		for rows.Next() {
			var c ContractResponse
			// created_at is timestamptz; pgx v5's default binary protocol
			// cannot scan it directly into *string (issue #220 — this
			// endpoint had no tests before, so a request here always 500'd).
			// Scan into time.Time and format explicitly, matching every
			// other handler's created_at handling (apikeys.go, admin.go).
			var createdAt time.Time
			if err := rows.Scan(&c.ID, &c.ContractID, &c.Network, &c.Label, &c.IndexFrom, &createdAt); err != nil {
				slog.ErrorContext(r.Context(), "failed to scan contract row", "err", err)
				httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "scan error")
				return
			}
			c.CreatedAt = createdAt.UTC().Format(time.RFC3339)
			contracts = append(contracts, c)
		}

		hasMore := len(contracts) > limit
		var nextCursor *string
		if hasMore {
			encoded := cursor.Encode(contracts[limit-1].ID)
			nextCursor = &encoded
			contracts = contracts[:limit]
		}

		writeJSON(w, http.StatusOK, ListContractsResponse{
			Contracts:  contracts,
			HasMore:    hasMore,
			NextCursor: nextCursor,
		})
	}
}

// DeleteContract handles DELETE /v1/admin/contracts/{id}.
// Admin-auth required. Idempotent — deleting a non-existent contract returns 204.
func DeleteContract(cfg ContractConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cfg.AdminKey == "" || cfg.DB == nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusServiceUnavailable, httputil.UNAVAILABLE, "admin contracts endpoint is not configured")
			return
		}

		if !validAdminKey(cfg.AdminKey, r.Header.Get("X-Admin-Key")) {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusUnauthorized, httputil.UNAUTHORIZED, "invalid or missing admin key")
			return
		}

		id := r.PathValue("id")
		if id == "" {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, "missing contract id")
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		tag, err := cfg.DB.Exec(ctx, `DELETE FROM indexed_contracts WHERE id = $1`, id)
		if err != nil {
			slog.ErrorContext(r.Context(), "failed to delete contract", "err", err)
			httputil.WriteErrorCtx(r.Context(), w, http.StatusInternalServerError, httputil.INTERNAL, "failed to delete contract")
			return
		}

		if tag.RowsAffected() == 0 {
			// Idempotent: already deleted or never existed.
			w.WriteHeader(http.StatusNoContent)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func parseInt(s string) (int, error) {
	var n int
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a number")
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

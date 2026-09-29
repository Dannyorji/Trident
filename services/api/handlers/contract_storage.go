package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/Depo-dev/trident/services/api/cursor"
	"github.com/Depo-dev/trident/services/api/internal/httputil"
	"github.com/Depo-dev/trident/services/api/middleware"
	"github.com/Depo-dev/trident/services/api/validation"
	"github.com/jackc/pgx/v5"
)

// contractStorageQueryTimeout bounds the DB calls in ContractStorageLatest/
// ContractStorageHistory so a runaway query can't hold a pool connection for
// the request's full budget (issue #238).
const contractStorageQueryTimeout = 5 * time.Second

// ContractStorageValue is one contract-storage key's value at a given ledger
// (issue #270).
type ContractStorageValue struct {
	StorageKey     string          `json:"storage_key"`
	Key            json.RawMessage `json:"key"`
	Value          json.RawMessage `json:"value"`
	LedgerSequence int64           `json:"ledger_sequence"`
	ObservedAt     time.Time       `json:"observed_at"`
}

// ContractStorageResponse lists the latest known value for every storage key
// snapshotted for a tracked contract (issue #270).
type ContractStorageResponse struct {
	ContractID string                 `json:"contract_id"`
	Network    string                 `json:"network"`
	Values     []ContractStorageValue `json:"values"`
}

// ContractStorageLatest returns the most recently observed value for every
// storage key snapshotted for a tracked contract (issue #270).
func ContractStorageLatest(db SchemaRegistryDB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		contractID := r.PathValue("id")
		if verr := validation.ValidateRequiredContractID("id", contractID); verr != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, verr.Message)
			return
		}
		if db == nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusServiceUnavailable, httputil.INTERNAL, "contract storage store unavailable")
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), contractStorageQueryTimeout)
		defer cancel()

		network := middleware.NetworkFromContext(r.Context())
		rows, err := db.Query(ctx, `
            SELECT DISTINCT ON (storage_key)
                storage_key, key_json, value_json, ledger_sequence, created_at
            FROM contract_storage_snapshots
            WHERE contract_id = $1 AND network = $2
            ORDER BY storage_key, ledger_sequence DESC
        `, contractID, network)
		if err != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusServiceUnavailable, httputil.INTERNAL, "failed to load contract storage")
			return
		}
		defer rows.Close()

		values := []ContractStorageValue{}
		for rows.Next() {
			var v ContractStorageValue
			var keyRaw, valueRaw []byte
			if err := rows.Scan(&v.StorageKey, &keyRaw, &valueRaw, &v.LedgerSequence, &v.ObservedAt); err != nil {
				httputil.WriteErrorCtx(r.Context(), w, http.StatusServiceUnavailable, httputil.INTERNAL, "failed to load contract storage")
				return
			}
			v.Key = json.RawMessage(keyRaw)
			if valueRaw != nil {
				v.Value = json.RawMessage(valueRaw)
			}
			values = append(values, v)
		}
		if err := rows.Err(); err != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusServiceUnavailable, httputil.INTERNAL, "failed to load contract storage")
			return
		}

		writeJSON(w, http.StatusOK, ContractStorageResponse{
			ContractID: contractID,
			Network:    network,
			Values:     values,
		})
	}
}

// ContractStorageHistoryResponse is the response envelope for GET /v1/contracts/{id}/storage/history.
//
// contract_id and network are retained from ContractStorageResponse so the
// history endpoint stays wire-compatible with the shape clients already
// consume; storage_key, has_more and next_cursor are additive.
type ContractStorageHistoryResponse struct {
	ContractID string                 `json:"contract_id"`
	Network    string                 `json:"network"`
	StorageKey string                 `json:"storage_key"`
	Values     []ContractStorageValue `json:"values"`
	HasMore    bool                   `json:"has_more"`
	NextCursor *string                `json:"next_cursor"`
}

// ContractStorageHistory returns every recorded change for a single storage
// key, oldest first, for a tracked contract (issue #270).
func ContractStorageHistory(db SchemaRegistryDB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		contractID := r.PathValue("id")
		if verr := validation.ValidateRequiredContractID("id", contractID); verr != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, verr.Message)
			return
		}
		storageKey := r.URL.Query().Get("key")
		if storageKey == "" {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, "key query parameter is required")
			return
		}

		q := r.URL.Query()
		if verr := validation.RejectUnknownParams(q, "key", "limit", "cursor"); verr != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, verr.Message)
			return
		}

		limit, verr := validation.ValidateLimit("limit", q.Get("limit"), 1, 200, 50)
		if verr != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, verr.Message)
			return
		}

		pagingToken, verr := validation.ValidateCursor("cursor", q.Get("cursor"))
		if verr != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, verr.Message)
			return
		}

		if db == nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusServiceUnavailable, httputil.INTERNAL, "contract storage store unavailable")
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), contractStorageQueryTimeout)
		defer cancel()

		network := middleware.NetworkFromContext(r.Context())

		// Keyset pagination: filter by ledger_sequence > cursor when provided.
		var rows pgx.Rows
		var queryErr error

		if pagingToken != "" {
			cursorLedger, parseErr := strconv.ParseInt(pagingToken, 10, 64)
			if parseErr != nil {
				httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, "invalid cursor")
				return
			}
			rows, queryErr = db.Query(ctx, `
				SELECT storage_key, key_json, value_json, ledger_sequence, created_at
				FROM contract_storage_snapshots
				WHERE contract_id = $1 AND network = $2 AND storage_key = $3 AND ledger_sequence > $4
				ORDER BY ledger_sequence ASC
				LIMIT $5
			`, contractID, network, storageKey, cursorLedger, limit+1)
		} else {
			rows, queryErr = db.Query(ctx, `
				SELECT storage_key, key_json, value_json, ledger_sequence, created_at
				FROM contract_storage_snapshots
				WHERE contract_id = $1 AND network = $2 AND storage_key = $3
				ORDER BY ledger_sequence ASC
				LIMIT $4
			`, contractID, network, storageKey, limit+1)
		}

		if queryErr != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusServiceUnavailable, httputil.INTERNAL, "failed to load contract storage history")
			return
		}
		defer rows.Close()

		values := []ContractStorageValue{}
		for rows.Next() {
			var v ContractStorageValue
			var keyRaw, valueRaw []byte
			if err := rows.Scan(&v.StorageKey, &keyRaw, &valueRaw, &v.LedgerSequence, &v.ObservedAt); err != nil {
				httputil.WriteErrorCtx(r.Context(), w, http.StatusServiceUnavailable, httputil.INTERNAL, "failed to load contract storage history")
				return
			}
			v.Key = json.RawMessage(keyRaw)
			if valueRaw != nil {
				v.Value = json.RawMessage(valueRaw)
			}
			values = append(values, v)
		}
		if err := rows.Err(); err != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusServiceUnavailable, httputil.INTERNAL, "failed to load contract storage history")
			return
		}

		hasMore := len(values) > int(limit)
		if hasMore {
			values = values[:limit]
		}

		var nextCursor *string
		if hasMore && len(values) > 0 {
			encoded := cursor.Encode(strconv.FormatInt(values[len(values)-1].LedgerSequence, 10))
			nextCursor = &encoded
		}

		writeJSON(w, http.StatusOK, ContractStorageHistoryResponse{
			ContractID: contractID,
			Network:    network,
			StorageKey: storageKey,
			Values:     values,
			HasMore:    hasMore,
			NextCursor: nextCursor,
		})
	}
}

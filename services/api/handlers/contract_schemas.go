package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Depo-dev/trident/services/api/internal/httputil"
	"github.com/Depo-dev/trident/services/api/middleware"
	"github.com/Depo-dev/trident/services/api/validation"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// schemaQueryTimeout bounds the DB calls in ContractEventSchemas so a
// runaway query can't hold a pool connection for the request's full budget
// (issue #238).
const schemaQueryTimeout = 5 * time.Second

const unknownSchemaCodeHash = "unknown"

type SchemaRegistryDB interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

type ContractEventFieldSchema struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type ContractEventSchema struct {
	EventName string                     `json:"event_name"`
	Fields    []ContractEventFieldSchema `json:"fields"`
	Source    string                     `json:"-"`
}

type ContractEventSchemaResponse struct {
	ContractID string                `json:"contract_id"`
	Network    string                `json:"network"`
	CodeHash   string                `json:"code_hash"`
	Events     []ContractEventSchema `json:"events"`
}

var knownContractEventSchemas = map[string][]ContractEventFieldSchema{
	"approve": {
		{Name: "from", Type: "address"},
		{Name: "spender", Type: "address"},
		{Name: "amount", Type: "i128"},
		{Name: "expiration_ledger", Type: "u32"},
	},
	"burn": {
		{Name: "from", Type: "address"},
		{Name: "amount", Type: "i128"},
	},
	"clawback": {
		{Name: "admin", Type: "address"},
		{Name: "from", Type: "address"},
		{Name: "amount", Type: "i128"},
	},
	"increase_supply": {
		{Name: "admin", Type: "address"},
		{Name: "amount", Type: "i128"},
	},
	"mint": {
		{Name: "admin", Type: "address"},
		{Name: "to", Type: "address"},
		{Name: "amount", Type: "i128"},
	},
	"set_admin": {
		{Name: "admin", Type: "address"},
		{Name: "new_admin", Type: "address"},
	},
	"set_authorized": {
		{Name: "admin", Type: "address"},
		{Name: "id", Type: "address"},
		{Name: "authorize", Type: "bool"},
	},
	"transfer": {
		{Name: "from", Type: "address"},
		{Name: "to", Type: "address"},
		{Name: "amount", Type: "i128"},
	},
}

// ContractEventSchemas observes a contract's event schemas from the indexed
// event tables and persists them to contract_event_schemas on every call
// (see persistContractSchemas). Because it writes on every request, this
// handler must never be wrapped in middleware.ResponseCache: a cache HIT
// would skip the write for the rest of the TTL, silently decoupling schema
// persistence from cache state (issue #571).
func ContractEventSchemas(db SchemaRegistryDB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		contractID := r.PathValue("id")
		if verr := validation.ValidateRequiredContractID("id", contractID); verr != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, verr.Message)
			return
		}
		if db == nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusServiceUnavailable, httputil.INTERNAL, "schema registry unavailable")
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), schemaQueryTimeout)
		defer cancel()

		network := middleware.NetworkFromContext(r.Context())
		codeHash, err := resolveContractCodeHash(ctx, db, contractID, network)
		if err != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusServiceUnavailable, httputil.INTERNAL, "failed to load contract schema")
			return
		}

		schemas, err := observeContractSchemas(ctx, db, contractID, network)
		if err != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusServiceUnavailable, httputil.INTERNAL, "failed to load contract schema")
			return
		}
		if err := persistContractSchemas(ctx, db, contractID, network, codeHash, schemas); err != nil {
			httputil.WriteErrorCtx(r.Context(), w, http.StatusServiceUnavailable, httputil.INTERNAL, "failed to persist contract schema")
			return
		}

		sort.Slice(schemas, func(i, j int) bool {
			return schemas[i].EventName < schemas[j].EventName
		})

		writeJSON(w, http.StatusOK, ContractEventSchemaResponse{
			ContractID: contractID,
			Network:    network,
			CodeHash:   codeHash,
			Events:     schemas,
		})
	}
}

func resolveContractCodeHash(ctx context.Context, db SchemaRegistryDB, contractID, network string) (string, error) {
	var codeHash string
	err := db.QueryRow(ctx, `
        SELECT on_chain_hash
        FROM contract_verification
        WHERE contract_id = $1 AND network = $2
    `, contractID, network).Scan(&codeHash)
	switch {
	case err == nil && codeHash != "":
		return codeHash, nil
	case errors.Is(err, pgx.ErrNoRows), codeHash == "":
		return unknownSchemaCodeHash, nil
	default:
		return "", err
	}
}

func observeContractSchemas(ctx context.Context, db SchemaRegistryDB, contractID, network string) ([]ContractEventSchema, error) {
	observed := make(map[string]ContractEventSchema)

	tokenRows, err := db.Query(ctx, `
        SELECT DISTINCT event_type
        FROM token_events
        WHERE contract_id = $1 AND network = $2
        ORDER BY event_type
    `, contractID, network)
	if err != nil {
		return nil, err
	}
	defer tokenRows.Close()

	for tokenRows.Next() {
		var eventName string
		if err := tokenRows.Scan(&eventName); err != nil {
			return nil, err
		}
		if fields, ok := schemaForKnownEvent(eventName); ok {
			observed[eventName] = ContractEventSchema{
				EventName: eventName,
				Fields:    fields,
				Source:    "token_events",
			}
		}
	}
	if err := tokenRows.Err(); err != nil {
		return nil, err
	}

	topicRows, err := db.Query(ctx, `
        SELECT DISTINCT topic_0
        FROM soroban_events
        WHERE contract_id = $1
          AND network = $2
          AND event_type = 'contract'
          AND topic_0 IS NOT NULL
          AND topic_0 <> ''
        ORDER BY topic_0
    `, contractID, network)
	if err != nil {
		return nil, err
	}
	defer topicRows.Close()

	for topicRows.Next() {
		var eventName string
		if err := topicRows.Scan(&eventName); err != nil {
			return nil, err
		}
		if _, exists := observed[eventName]; exists {
			continue
		}

		fields, source, err := deriveSchemaForEvent(ctx, db, contractID, network, eventName)
		if err != nil {
			return nil, err
		}
		if len(fields) == 0 {
			continue
		}
		observed[eventName] = ContractEventSchema{
			EventName: eventName,
			Fields:    fields,
			Source:    source,
		}
	}
	if err := topicRows.Err(); err != nil {
		return nil, err
	}

	schemas := make([]ContractEventSchema, 0, len(observed))
	for _, schema := range observed {
		schemas = append(schemas, schema)
	}
	return schemas, nil
}

func schemaForKnownEvent(eventName string) ([]ContractEventFieldSchema, bool) {
	fields, ok := knownContractEventSchemas[eventName]
	if !ok {
		return nil, false
	}
	cloned := append([]ContractEventFieldSchema(nil), fields...)
	return cloned, true
}

func deriveSchemaForEvent(ctx context.Context, db SchemaRegistryDB, contractID, network, eventName string) ([]ContractEventFieldSchema, string, error) {
	if fields, ok := schemaForKnownEvent(eventName); ok {
		return fields, "observed_topics", nil
	}

	var raw []byte
	err := db.QueryRow(ctx, `
        SELECT data
        FROM soroban_events
        WHERE contract_id = $1 AND network = $2 AND topic_0 = $3
        ORDER BY ledger_sequence DESC, event_index DESC
        LIMIT 1
    `, contractID, network, eventName).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}

	fields, err := fieldsFromJSON(raw)
	if err != nil {
		return nil, "", err
	}
	return fields, "soroban_events", nil
}

func fieldsFromJSON(raw []byte) ([]ContractEventFieldSchema, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, nil
	}

	var payload any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}

	fields := fieldsFromValue(payload)
	sort.Slice(fields, func(i, j int) bool {
		return fields[i].Name < fields[j].Name
	})
	return fields, nil
}

func fieldsFromValue(payload any) []ContractEventFieldSchema {
	switch typed := payload.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			if key == "event" {
				continue
			}
			keys = append(keys, key)
		}
		sort.Strings(keys)

		fields := make([]ContractEventFieldSchema, 0, len(keys))
		for _, key := range keys {
			fields = append(fields, ContractEventFieldSchema{
				Name: key,
				Type: jsonSchemaType(typed[key]),
			})
		}
		return fields
	case []any:
		return []ContractEventFieldSchema{{Name: "items", Type: "array"}}
	case nil:
		return nil
	default:
		return []ContractEventFieldSchema{{Name: "value", Type: jsonSchemaType(typed)}}
	}
}

func jsonSchemaType(value any) string {
	switch value.(type) {
	case string:
		return "string"
	case bool:
		return "bool"
	case float64:
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	case nil:
		return "null"
	default:
		return "unknown"
	}
}

func persistContractSchemas(ctx context.Context, db SchemaRegistryDB, contractID, network, codeHash string, schemas []ContractEventSchema) error {
	eventNames := make([]string, 0, len(schemas))
	for _, schema := range schemas {
		eventNames = append(eventNames, schema.EventName)
	}

	if len(eventNames) == 0 {
		_, err := db.Exec(ctx, `
            DELETE FROM contract_event_schemas
            WHERE contract_id = $1 AND network = $2 AND code_hash = $3
        `, contractID, network, codeHash)
		return err
	}

	if _, err := db.Exec(ctx, `
        DELETE FROM contract_event_schemas
        WHERE contract_id = $1
          AND network = $2
          AND code_hash = $3
          AND NOT (event_name = ANY($4))
    `, contractID, network, codeHash, eventNames); err != nil {
		return err
	}

	for _, schema := range schemas {
		payload, err := json.Marshal(schema.Fields)
		if err != nil {
			return err
		}
		if _, err := db.Exec(ctx, `
            INSERT INTO contract_event_schemas (
                contract_id,
                network,
                event_name,
                code_hash,
                field_schema,
                observed_source
            ) VALUES ($1, $2, $3, $4, $5::jsonb, $6)
            ON CONFLICT (contract_id, network, event_name, code_hash)
            DO UPDATE SET
                field_schema = EXCLUDED.field_schema,
                observed_source = EXCLUDED.observed_source,
                updated_at = NOW()
        `, contractID, network, schema.EventName, codeHash, string(payload), schema.Source); err != nil {
			return err
		}
	}

	return nil
}

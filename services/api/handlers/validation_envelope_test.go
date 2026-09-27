package handlers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Depo-dev/trident/services/api/handlers"
	"github.com/Depo-dev/trident/services/api/middleware"
	"github.com/alicebob/miniredis/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/redis/go-redis/v9"
)

// stubDBPool satisfies handlers.DBPool. Validation runs before any query, so
// these methods are never reached on the paths under test.
type stubDBPool struct{}

func (stubDBPool) QueryRow(context.Context, string, ...any) pgx.Row { return nil }
func (stubDBPool) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, nil
}
func (stubDBPool) Ping(context.Context) error { return nil }

// Every bad-input path must answer with the canonical envelope
// {"error":{"code":"INVALID_ARGUMENT","message":...}} and name the offending
// field in the message (issue #222).

const testContractID = "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"

type errorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// assertInvalidArgument checks the status, the machine-readable code and that
// the message points at wantField.
func assertInvalidArgument(t *testing.T, rec *httptest.ResponseRecorder, wantField string) {
	t.Helper()

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want %d (body: %s)", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type: got %q, want application/json", ct)
	}

	var body errorEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Error.Code != "INVALID_ARGUMENT" {
		t.Errorf("code: got %q, want INVALID_ARGUMENT", body.Error.Code)
	}
	if body.Error.Message == "" {
		t.Error("message must not be empty")
	}
	if wantField != "" && !strings.Contains(body.Error.Message, wantField) {
		t.Errorf("message %q should name field %q", body.Error.Message, wantField)
	}
}

func TestStreamBadInputReturnsCanonicalEnvelope(t *testing.T) {
	tests := []struct {
		name      string
		query     string
		wantField string
	}{
		{"missing contractId", "", "contractId"},
		{"malformed contractId", "?contractId=not-a-strkey", "contractId"},
		{"lowercase contractId", "?contractId=" + strings.ToLower(testContractID), "contractId"},
		{"unknown parameter", "?contractId=" + testContractID + "&topic9=x", "topic9"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/events/stream"+tt.query, nil)
			rec := httptest.NewRecorder()

			handlers.Stream(nil)(rec, req)

			assertInvalidArgument(t, rec, tt.wantField)
		})
	}
}

func TestListEventsBadInputReturnsCanonicalEnvelope(t *testing.T) {
	tests := []struct {
		name      string
		query     string
		wantField string
	}{
		{"limit below minimum", "?limit=0", "limit"},
		{"limit above maximum", "?limit=201", "limit"},
		{"limit not a number", "?limit=ten", "limit"},
		{"negative ledgerFrom", "?ledgerFrom=-1", "ledgerFrom"},
		{"inverted ledger range", "?ledgerFrom=20&ledgerTo=10", "ledgerTo"},
		{"malformed contractId", "?contractId=nope", "contractId"},
		{"unknown event_type", "?event_type=transfer", "event_type"},
		{"malformed cursor", "?cursor=!!!not-a-cursor!!!", "cursor"},
		{"unknown parameter", "?limitt=5", "limitt"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/events"+tt.query, nil)
			rec := httptest.NewRecorder()

			handlers.ListEvents(rec, req)

			assertInvalidArgument(t, rec, tt.wantField)
		})
	}
}

func TestGetEventBadIDReturnsCanonicalEnvelope(t *testing.T) {
	for _, id := range []string{"", "not-a-uuid", "550e8400-e29b-11d4-a716-446655440000"} {
		t.Run("id="+id, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/events/"+id, nil)
			req.SetPathValue("id", id)
			rec := httptest.NewRecorder()

			handlers.GetEvent(rec, req)

			assertInvalidArgument(t, rec, "id")
		})
	}
}

func TestContractsStatsBadInputReturnsCanonicalEnvelope(t *testing.T) {
	tests := []struct {
		name      string
		query     string
		wantField string
	}{
		{"unknown network", "?network=futurenet", "network"},
		{"limit above maximum", "?limit=101", "limit"},
		{"negative from_ledger", "?from_ledger=-3", "from_ledger"},
		{"inverted ledger range", "?from_ledger=90&to_ledger=10", "to_ledger"},
		{"unknown parameter", "?sort=desc", "sort"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/stats/contracts"+tt.query, nil)
			rec := httptest.NewRecorder()

			handlers.ContractsStats(stubDBPool{}, nil)(rec, req)

			assertInvalidArgument(t, rec, tt.wantField)
		})
	}
}

// TestContractsStatsRejectsClientSuppliedNetwork_EvenWhenSyntacticallyValid
// guards against the network isolation bypass in issue #612: unlike
// TestContractsStatsBadInputReturnsCanonicalEnvelope's "unknown network" case
// (?network=futurenet, rejected because "futurenet" isn't a valid network
// value), this asserts that ?network=mainnet — a syntactically *valid*
// network — is rejected too, because network is not a query parameter at all
// on this route. Network is derived exclusively from the authenticated key's
// context (middleware.NetworkFromContext); a testnet-scoped key could
// otherwise read mainnet contract aggregates simply by passing
// ?network=mainnet on the query string.
func TestContractsStatsRejectsClientSuppliedNetwork_EvenWhenSyntacticallyValid(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/stats/contracts?network=mainnet", nil)
	rec := httptest.NewRecorder()

	handlers.ContractsStats(stubDBPool{}, nil)(rec, req)

	assertInvalidArgument(t, rec, "network")
}

// noRowsDBPool is a zero-row handlers.DBPool stand-in, local to this file
// (handlers_test can't reach contract_xcache_test.go's package-internal
// xcacheMissDB), used only to let ContractsStats reach its response-encoding
// step without a real database.
type noRowsDBPool struct{}

func (noRowsDBPool) Ping(context.Context) error { return nil }
func (noRowsDBPool) QueryRow(context.Context, string, ...any) pgx.Row {
	return nil
}
func (noRowsDBPool) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return &noRowsRows{}, nil
}

type noRowsRows struct{}

func (noRowsRows) Close()                                       {}
func (noRowsRows) Err() error                                   { return nil }
func (noRowsRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (noRowsRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (noRowsRows) Next() bool                                   { return false }
func (noRowsRows) Scan(...any) error                            { return nil }
func (noRowsRows) Values() ([]any, error)                       { return nil, nil }
func (noRowsRows) RawValues() [][]byte                          { return nil }
func (noRowsRows) Conn() *pgx.Conn                              { return nil }

// TestContractsStatsUsesNetworkFromContext_NotQueryString proves the positive
// half of issue #612's fix: a request authenticated on testnet gets testnet
// aggregates regardless of what (if anything) a client puts in the query
// string, because ContractsStats never reads r.URL.Query().Get("network").
func TestContractsStatsUsesNetworkFromContext_NotQueryString(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	req := httptest.NewRequest(http.MethodGet, "/v1/stats/contracts?from_ledger=0&to_ledger=1000000&limit=10", nil)
	req = req.WithContext(middleware.WithNetwork(req.Context(), "testnet"))
	rec := httptest.NewRecorder()

	handlers.ContractsStats(noRowsDBPool{}, rdb)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Network string `json:"network"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Network != "testnet" {
		t.Fatalf("response network: got %q, want %q (derived from context, not the query string)", body.Network, "testnet")
	}
}

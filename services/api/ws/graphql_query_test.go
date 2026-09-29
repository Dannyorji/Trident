package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Depo-dev/trident/services/api/internal/httputil"
)

// Resolver and transport tests for GraphQL query parity (issue #223).

// fakeBackend is a scriptable EventsBackend. Each field records what it was
// called with, so a test can assert the resolver passed REST's filters
// through rather than dropping them.
type fakeBackend struct {
	page      EventsPage
	event     map[string]any
	stats     []map[string]any
	err       error
	gotQuery  EventsQuery
	gotStats  StatsQuery
	gotID     string
	gotNetwrk string
	calls     int
}

func (f *fakeBackend) ListEvents(_ context.Context, req EventsQuery) (EventsPage, error) {
	f.calls++
	f.gotQuery = req
	if f.err != nil {
		return EventsPage{}, f.err
	}
	return f.page, nil
}

func (f *fakeBackend) GetEvent(_ context.Context, id, network string) (map[string]any, error) {
	f.calls++
	f.gotID, f.gotNetwrk = id, network
	if f.err != nil {
		return nil, f.err
	}
	return f.event, nil
}

func (f *fakeBackend) ContractStats(_ context.Context, req StatsQuery) ([]map[string]any, error) {
	f.calls++
	f.gotStats = req
	if f.err != nil {
		return nil, f.err
	}
	return f.stats, nil
}

func payload(t *testing.T, query string, vars map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return b
}

// ---------------------------------------------------------------------------
// Operation dispatch
// ---------------------------------------------------------------------------

func TestGQLResolve_EventsQuery(t *testing.T) {
	cursor := "b3BhcXVl"
	backend := &fakeBackend{page: EventsPage{
		Events:     []map[string]any{{"id": "e1", "contract_id": "C1", "ledger_sequence": 42}},
		HasMore:    true,
		NextCursor: &cursor,
	}}

	op, err := gqlParseQuery(payload(t, `query { events(contractId: "CABC", limit: 10) { events { id } } }`, nil))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	data, err := gqlResolveQuery(context.Background(), backend, op, "mainnet")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	events, ok := data["events"].(map[string]any)
	if !ok {
		t.Fatalf("expected an events object, got %#v", data)
	}
	if events["hasMore"] != true {
		t.Errorf("hasMore = %v, want true", events["hasMore"])
	}
	if events["nextCursor"] != cursor {
		t.Errorf("nextCursor = %v, want %q", events["nextCursor"], cursor)
	}
	// The event must come back under the camelCase schema names, matching
	// what the subscription transport emits for the same event.
	list := events["events"].([]any)
	first := list[0].(map[string]any)
	if first["contractId"] != "C1" {
		t.Errorf("contractId = %v, want C1 (snake_case must be remapped)", first["contractId"])
	}
	if _, leaked := first["contract_id"]; leaked {
		t.Error("raw snake_case contract_id leaked into the GraphQL payload")
	}
}

// TestGQLResolve_EventsPassesRESTFilters is the parity check on inputs: every
// filter GET /v1/events accepts must reach the backend.
func TestGQLResolve_EventsPassesRESTFilters(t *testing.T) {
	backend := &fakeBackend{}
	op, err := gqlParseQuery(payload(t, `query { events { events { id } } }`, map[string]any{
		"contractId": "CABC",
		"topic0":     "t0",
		"topic1":     "t1",
		"eventType":  "contract",
		"ledgerFrom": 100,
		"ledgerTo":   200,
		"cursor":     "cur",
		"limit":      25,
	}))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := gqlResolveQuery(context.Background(), backend, op, "mainnet"); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	got := backend.gotQuery
	if got.ContractID != "CABC" || got.Topic0 != "t0" || got.Topic1 != "t1" {
		t.Errorf("contract/topic filters lost: %+v", got)
	}
	if got.EventType != "contract" || got.Cursor != "cur" || got.Limit != 25 {
		t.Errorf("eventType/cursor/limit lost: %+v", got)
	}
	if got.LedgerFrom == nil || *got.LedgerFrom != 100 || got.LedgerTo == nil || *got.LedgerTo != 200 {
		t.Errorf("ledger range lost: %+v", got)
	}
}

// TestGQLResolve_NetworkComesFromConnection is the security-relevant half of
// parity: the network is taken from the authenticated key, never from the
// query, exactly as the REST handlers enforce it.
func TestGQLResolve_NetworkComesFromConnection(t *testing.T) {
	backend := &fakeBackend{}
	op, err := gqlParseQuery(payload(t, `query { events { events { id } } }`, map[string]any{
		"network": "mainnet", // a caller trying to override it
	}))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := gqlResolveQuery(context.Background(), backend, op, "testnet"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if backend.gotQuery.Network != "testnet" {
		t.Fatalf("network = %q, want testnet — a query argument must not override the connection's network",
			backend.gotQuery.Network)
	}
}

func TestGQLResolve_SingleEvent(t *testing.T) {
	backend := &fakeBackend{event: map[string]any{"id": "e1", "contract_id": "C1"}}
	op, err := gqlParseQuery(payload(t, `query { event(id: "11111111-1111-4111-8111-111111111111") { id } }`, nil))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	data, err := gqlResolveQuery(context.Background(), backend, op, "testnet")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	ev, ok := data["event"].(map[string]any)
	if !ok {
		t.Fatalf("expected an event object, got %#v", data)
	}
	if ev["contractId"] != "C1" {
		t.Errorf("contractId = %v, want C1", ev["contractId"])
	}
	if backend.gotID != "11111111-1111-4111-8111-111111111111" {
		t.Errorf("id not forwarded: %q", backend.gotID)
	}
}

// A missing event resolves to a null field rather than a transport error,
// which is the GraphQL convention for "no such object".
func TestGQLResolve_SingleEventNotFound(t *testing.T) {
	backend := &fakeBackend{event: nil}
	op, _ := gqlParseQuery(payload(t, `query { event(id: "11111111-1111-4111-8111-111111111111") { id } }`, nil))
	data, err := gqlResolveQuery(context.Background(), backend, op, "testnet")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if v, ok := data["event"]; !ok || v != nil {
		t.Fatalf("event = %#v, want an explicit null", data["event"])
	}
}

func TestGQLResolve_ContractStats(t *testing.T) {
	backend := &fakeBackend{stats: []map[string]any{
		{"contract_id": "C1", "event_count": int64(7)},
	}}
	op, err := gqlParseQuery(payload(t, `query { contractStats(limit: 5) { contractId eventCount } }`, nil))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	data, err := gqlResolveQuery(context.Background(), backend, op, "mainnet")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	rows := data["contractStats"].([]any)
	if len(rows) != 1 {
		t.Fatalf("got %d stats rows, want 1", len(rows))
	}
	row := rows[0].(map[string]any)
	if row["contractId"] != "C1" || row["eventCount"] != int64(7) {
		t.Errorf("stats row = %#v", row)
	}
	if backend.gotStats.Network != "mainnet" || backend.gotStats.Limit != 5 {
		t.Errorf("stats query = %+v", backend.gotStats)
	}
}

// ---------------------------------------------------------------------------
// Error taxonomy
// ---------------------------------------------------------------------------

// TestGQLResolve_ErrorsUseCanonicalCodes is the acceptance criterion "errors
// mapped to canonical codes".
func TestGQLResolve_ErrorsUseCanonicalCodes(t *testing.T) {
	cases := []struct {
		name    string
		query   string
		vars    map[string]any
		backend EventsBackend
		want    httputil.ErrorCode
	}{
		{
			name:    "unknown operation",
			query:   `query { nonsense { id } }`,
			backend: &fakeBackend{},
			want:    httputil.INVALID_ARGUMENT,
		},
		{
			name:    "limit above the REST ceiling",
			query:   `query { events { events { id } } }`,
			vars:    map[string]any{"limit": gqlMaxPageSize + 1},
			backend: &fakeBackend{},
			want:    httputil.INVALID_ARGUMENT,
		},
		{
			name:    "limit below one",
			query:   `query { events { events { id } } }`,
			vars:    map[string]any{"limit": 0},
			backend: &fakeBackend{},
			want:    httputil.INVALID_ARGUMENT,
		},
		{
			name:    "inverted ledger range",
			query:   `query { events { events { id } } }`,
			vars:    map[string]any{"ledgerFrom": 500, "ledgerTo": 100},
			backend: &fakeBackend{},
			want:    httputil.INVALID_ARGUMENT,
		},
		{
			name:    "missing id on single event",
			query:   `query { event { id } }`,
			backend: &fakeBackend{},
			want:    httputil.INVALID_ARGUMENT,
		},
		{
			name:    "backend unavailable",
			query:   `query { events { events { id } } }`,
			backend: nil,
			want:    httputil.UNAVAILABLE,
		},
		{
			name:    "backend error keeps its code",
			query:   `query { events { events { id } } }`,
			backend: &fakeBackend{err: NewBackendError(httputil.NOT_FOUND, "nope")},
			want:    httputil.NOT_FOUND,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			op, err := gqlParseQuery(payload(t, tc.query, tc.vars))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			_, rerr := gqlResolveQuery(context.Background(), tc.backend, op, "testnet")
			if rerr == nil {
				t.Fatal("expected an error")
			}
			var ge *gqlError
			if !errorsAs(rerr, &ge) {
				t.Fatalf("error %v does not carry a canonical code", rerr)
			}
			if ge.code != tc.want {
				t.Errorf("code = %s, want %s", ge.code, tc.want)
			}
		})
	}
}

// TestGQLCodedErrorMsg_PutsCodeInExtensions checks the wire shape: the SDKs
// read the machine-readable classification from errors[].extensions.code.
func TestGQLCodedErrorMsg_PutsCodeInExtensions(t *testing.T) {
	msg := gqlCodedErrorMsg("op1", gqlErrf(httputil.RATE_LIMITED, "rate limit exceeded"))
	if msg.Type != "error" || msg.ID != "op1" {
		t.Fatalf("envelope = %+v", msg)
	}
	var errs []struct {
		Message    string `json:"message"`
		Extensions struct {
			Code string `json:"code"`
		} `json:"extensions"`
	}
	if err := json.Unmarshal(msg.Payload, &errs); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if len(errs) != 1 || errs[0].Extensions.Code != string(httputil.RATE_LIMITED) {
		t.Fatalf("payload = %s", msg.Payload)
	}
	if errs[0].Message != "rate limit exceeded" {
		t.Errorf("message = %q", errs[0].Message)
	}
}

// An unclassified error must not leak backend detail into a client message.
func TestGQLCodedErrorMsg_UnclassifiedIsInternal(t *testing.T) {
	msg := gqlCodedErrorMsg("op1", fmt.Errorf("pq: relation \"secret_table\" does not exist"))
	if strings.Contains(string(msg.Payload), "secret_table") {
		t.Fatalf("backend detail leaked to the client: %s", msg.Payload)
	}
}

// ---------------------------------------------------------------------------
// Query vs subscription routing
// ---------------------------------------------------------------------------

// TestGQLIsSubscription covers the routing decision the protocol loop makes,
// including the compatibility case: a bare selection set naming the
// subscription root field must still stream, because that is what clients
// written before #223 send.
func TestGQLIsSubscription(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  bool
	}{
		{"explicit subscription", `subscription { contractEvents(contractId: "C1") { id } }`, true},
		{"named subscription operation", `subscription($cid:String!){contractEvents(contractId:$cid){id}}`, true},
		{"bare selection of the subscription field", `{ contractEvents(contractId: "C1") { id } }`, true},
		{"explicit query", `query { events { events { id } } }`, false},
		{"bare selection of a query field", `{ events { events { id } } }`, false},
		{"single event query", `query { event(id: "x") { id } }`, false},
		{"stats query", `query { contractStats { contractId } }`, false},

		// Subscription is the default: this transport carried only
		// subscriptions before #223, so anything that does not positively
		// identify itself as a query keeps streaming. An unrecognised
		// document must not be silently answered once and completed.
		{"empty document", ``, true},
		{"whitespace only", `   `, true},
		{"unrecognised root field", `{ somethingElse { id } }`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := gqlIsSubscription(payload(t, tc.query, nil))
			if got != tc.want {
				t.Errorf("gqlIsSubscription(%q) = %v, want %v", tc.query, got, tc.want)
			}
		})
	}
}

// TestGQLIsSubscription_LegacyPayloadShapes pins the compatibility contract
// directly. Clients written before #223 subscribe with contractId in
// `variables` and frequently no `query` field at all; routing one of those to
// the query path would answer a stream request with a single result and
// complete the operation. The existing subscription tests send exactly this
// shape.
func TestGQLIsSubscription_LegacyPayloadShapes(t *testing.T) {
	cases := []struct {
		name    string
		payload json.RawMessage
	}{
		{"variables only, no query field", json.RawMessage(`{"variables":{"contractId":"C1"}}`)},
		{"empty object", json.RawMessage(`{}`)},
		{"nil payload", nil},
		{"malformed payload", json.RawMessage(`{not json`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !gqlIsSubscription(tc.payload) {
				t.Error("must be treated as a subscription — routing it to the query path " +
					"would answer a stream request with one result and complete it")
			}
		})
	}
}

func TestGQLParseQuery_RejectsOversizeAndEmpty(t *testing.T) {
	long := strings.Repeat("a", gqlMaxQueryLen+1)
	if _, err := gqlParseQuery(payload(t, long, nil)); err == nil {
		t.Error("an over-long query must be rejected outright (issue #317)")
	}
	if _, err := gqlParseQuery(payload(t, "   ", nil)); err == nil {
		t.Error("an empty query must be rejected")
	}
	if _, err := gqlParseQuery(nil); err == nil {
		t.Error("a missing payload must be rejected")
	}
}

// Variables take precedence over inline arguments, matching the precedence
// rule gqlParseSubscribe already applied.
func TestGQLParseQuery_VariablesOverrideInlineArgs(t *testing.T) {
	op, err := gqlParseQuery(payload(t,
		`query { events(contractId: "CINLINE", limit: 5) { events { id } } }`,
		map[string]any{"contractId": "CVAR", "limit": 50},
	))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := gqlString(op.variables, "contractId"); got != "CVAR" {
		t.Errorf("contractId = %q, want CVAR", got)
	}
	if n, _ := gqlInt(op.variables, "limit"); n != 50 {
		t.Errorf("limit = %d, want 50", n)
	}
}

// ---------------------------------------------------------------------------
// Auth and rate limiting
// ---------------------------------------------------------------------------

func TestGQLAuthenticate_UnconfiguredAllowsDefaultNetwork(t *testing.T) {
	keyID, network, ok := gqlAuthenticate(context.Background(), nil, "anything")
	if !ok {
		t.Fatal("with no auth configured the connection must be allowed")
	}
	if keyID != "" || network != gqlDefaultNetwork {
		t.Errorf("keyID=%q network=%q, want empty id on %s", keyID, network, gqlDefaultNetwork)
	}
}

func TestGQLAuthenticate_RejectsBadKeyAndCarriesNetwork(t *testing.T) {
	auth := func(_ context.Context, key string) (string, string, bool) {
		if key != "good" {
			return "", "", false
		}
		return "key-1", "mainnet", true
	}

	if _, _, ok := gqlAuthenticate(context.Background(), auth, "bad"); ok {
		t.Error("an unknown key must be rejected")
	}
	keyID, network, ok := gqlAuthenticate(context.Background(), auth, "good")
	if !ok || keyID != "key-1" || network != "mainnet" {
		t.Errorf("keyID=%q network=%q ok=%v", keyID, network, ok)
	}
}

// An authenticated key with no network recorded falls back to the same
// default an unauthenticated REST request gets.
func TestGQLAuthenticate_EmptyNetworkDefaults(t *testing.T) {
	auth := func(_ context.Context, _ string) (string, string, bool) { return "key-1", "", true }
	_, network, ok := gqlAuthenticate(context.Background(), auth, "k")
	if !ok || network != gqlDefaultNetwork {
		t.Errorf("network = %q, want %s", network, gqlDefaultNetwork)
	}
}

func TestGQLCheckRateLimit(t *testing.T) {
	t.Run("no limiter allows", func(t *testing.T) {
		if ok, _ := gqlCheckRateLimit(context.Background(), nil, "key-1"); !ok {
			t.Error("no limiter configured must allow")
		}
	})

	t.Run("denies with RATE_LIMITED", func(t *testing.T) {
		limiter := func(context.Context, string) (bool, error) { return false, nil }
		ok, err := gqlCheckRateLimit(context.Background(), limiter, "key-1")
		if ok {
			t.Fatal("limiter denied but the operation was allowed")
		}
		var ge *gqlError
		if !errorsAs(err, &ge) || ge.code != httputil.RATE_LIMITED {
			t.Fatalf("error = %v, want a RATE_LIMITED coded error", err)
		}
	})

	// Matching TieredRateLimit's documented fail-open behaviour: a Redis blip
	// must not take GraphQL down while REST keeps serving.
	t.Run("fails open on limiter error", func(t *testing.T) {
		limiter := func(context.Context, string) (bool, error) {
			return false, fmt.Errorf("redis down")
		}
		if ok, _ := gqlCheckRateLimit(context.Background(), limiter, "key-1"); !ok {
			t.Error("a limiter error must fail open, as the REST limiter does")
		}
	})
}

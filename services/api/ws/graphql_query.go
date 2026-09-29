package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Depo-dev/trident/services/api/internal/httputil"
)

// GraphQL query support for the graphql-transport-ws transport (issue #223).
//
// Scope, and why it is not a general GraphQL engine
// -------------------------------------------------
// The issue explicitly puts "arbitrary ad-hoc GraphQL over the whole schema"
// out of scope and asks for parity on a named set of operations: the events
// query (same filters and pagination as REST), a single event, the event
// subscription that already existed, and contract stats.
//
// So this dispatches a fixed set of named operations rather than executing an
// arbitrary query tree, which is the same shape the existing subscribe path
// already had. That choice is what keeps the #317 abuse guards meaningful:
// with no general executor there is no query tree whose depth or complexity
// could be scored, and the things that can actually be abused — query length
// and concurrent operations per connection — are capped directly.
//
// Resolvers delegate to the same backends the REST handlers use, so parity is
// structural rather than reimplemented: the events query and single event go
// through the same gRPC client, and contract stats through the same database
// query, meaning a filter or a fix applies to both transports at once.

// reSubscriptionDoc matches a document whose operation type is an explicit
// `subscription`. graphql-transport-ws carries queries and subscriptions in
// the same "subscribe" message, so the document itself is what distinguishes
// them.
var reSubscriptionDoc = regexp.MustCompile(`(?i)^\s*subscription\b`)

// gqlIsSubscription reports whether a subscribe payload carries a streaming
// subscription rather than a one-shot query.
//
// Subscription is the default, and deliberately so: this transport served
// only subscriptions before #223, and those clients are not required to
// change. Anything that does not positively identify itself as a query keeps
// its old meaning — including a payload carrying only `variables` and no
// `query` at all, which is a shape the existing clients (and the pre-#223
// tests) send and which gqlParseSubscribe has always accepted by reading
// contractId out of the variables.
//
// Only two things route to the query path: an explicit `query` operation
// type, or an anonymous selection set whose root field is one of the
// supported query fields.
func gqlIsSubscription(payload json.RawMessage) bool {
	if payload == nil {
		return true
	}
	var p struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return true
	}

	doc := strings.TrimSpace(p.Query)
	if doc == "" {
		// No document: contractId comes from variables. This is the
		// pre-#223 subscribe shape.
		return true
	}
	if reSubscriptionDoc.MatchString(doc) {
		return true
	}
	if reQueryDoc.MatchString(doc) {
		return false
	}

	// Anonymous document: decide on the root field it selects, so a bare
	// `{ contractEvents(...) }` still streams while a bare `{ events {...} }`
	// resolves once.
	for _, field := range gqlSupportedQueries {
		if reRootSelects(doc, field) {
			return false
		}
	}
	return true
}

// reQueryDoc matches a document whose operation type is an explicit `query`.
var reQueryDoc = regexp.MustCompile(`(?i)^query\b`)

// reRootSelects reports whether doc's first selection is the named field.
func reRootSelects(doc, field string) bool {
	m := reOperationName.FindStringSubmatch(doc)
	return len(m) == 2 && m[1] == field
}

// gqlAuthenticate validates a connection_init key and returns the identity to
// attach to the connection.
//
// A nil auth function means authentication is not configured — the same
// posture middleware.Auth takes when its hash set is empty, and what keeps
// local development usable. The connection is then treated as authenticated
// on the default network, exactly as an unauthenticated REST request is.
func gqlAuthenticate(ctx context.Context, auth GraphQLAuthFunc, key string) (keyID, network string, ok bool) {
	if auth == nil {
		return "", gqlDefaultNetwork, true
	}
	keyID, network, ok = auth(ctx, key)
	if !ok {
		return "", "", false
	}
	if network == "" {
		network = gqlDefaultNetwork
	}
	return keyID, network, true
}

// gqlDefaultNetwork matches middleware.NetworkFromContext's default, so an
// unauthenticated GraphQL connection reads the same network an
// unauthenticated REST request does.
const gqlDefaultNetwork = "testnet"

// gqlCheckRateLimit consults the tiered limiter for one operation. When no
// limiter is configured the operation is allowed, matching how the REST chain
// behaves with no Redis backing the limiter.
//
// A limiter error fails open and is reported as allowed: the REST limiter
// makes the same choice (see middleware.TieredRateLimit, "rate limit check
// failed; failing open"), and diverging here would mean a Redis blip breaks
// GraphQL while REST keeps serving.
func gqlCheckRateLimit(ctx context.Context, limiter GraphQLRateLimiter, keyID string) (bool, error) {
	if limiter == nil || keyID == "" {
		return true, nil
	}
	allowed, err := limiter(ctx, keyID)
	if err != nil {
		return true, nil
	}
	if !allowed {
		return false, gqlErrf(httputil.RATE_LIMITED, "rate limit exceeded")
	}
	return true, nil
}

// gqlMaxPageSize mirrors the REST limit ceiling for GET /v1/events, so a
// GraphQL caller cannot request a larger page than a REST caller can.
const gqlMaxPageSize = 200

// gqlDefaultPageSize mirrors the REST default page size.
const gqlDefaultPageSize = 50

// gqlQueryTimeout bounds a single resolver's backend call. A GraphQL client
// holds a long-lived socket, so without this one slow backend call would pin
// a reader goroutine indefinitely.
const gqlQueryTimeout = 10 * time.Second

// EventsBackend is the subset of the events data source the GraphQL query
// resolvers need. services/api/handlers implements this over the same gRPC
// client the REST handlers use, so both transports resolve through one path.
type EventsBackend interface {
	ListEvents(ctx context.Context, req EventsQuery) (EventsPage, error)
	GetEvent(ctx context.Context, id, network string) (map[string]any, error)
	ContractStats(ctx context.Context, req StatsQuery) ([]map[string]any, error)
}

// EventsQuery carries the same filter and pagination inputs GET /v1/events
// accepts. Zero values mean "not filtered", matching REST's optional params.
type EventsQuery struct {
	ContractID string
	Topic0     string
	Topic1     string
	EventType  string
	LedgerFrom *int64
	LedgerTo   *int64
	Cursor     string
	Limit      int
	Network    string
}

// EventsPage is the paginated result of an events query — the GraphQL-side
// mirror of ListEventsResponse, carrying the same has_more/next_cursor
// contract so a GraphQL auto-pager stops on the same signal a REST one does.
type EventsPage struct {
	Events     []map[string]any
	HasMore    bool
	NextCursor *string
}

// StatsQuery carries the inputs GET /v1/stats/contracts accepts.
type StatsQuery struct {
	FromLedger int64
	ToLedger   int64
	Network    string
	Limit      int
}

// gqlOperation is one supported query, parsed out of a client payload.
type gqlOperation struct {
	name      string
	variables map[string]any
	// query is retained so field selection can be inspected without a full
	// parser; see gqlSelects.
	query string
}

var (
	// reOperationName matches the first root field in the selection set, which
	// for the supported single-operation queries is the operation itself:
	// `query { events(...) { ... } }` or `{ event(id: "...") { ... } }`.
	reOperationName = regexp.MustCompile(`(?s)\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*[({]`)
	reStringArg     = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\s*:\s*"([^"]*)"`)
	reNumberArg     = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\s*:\s*(-?\d+)\b`)
)

// gqlParseQuery extracts the operation name and its arguments from a
// graphql-transport-ws subscribe payload carrying a query (not a
// subscription). Variables take precedence over inline arguments, matching
// gqlParseSubscribe's existing precedence rule.
func gqlParseQuery(payload json.RawMessage) (*gqlOperation, error) {
	if payload == nil {
		return nil, fmt.Errorf("missing operation payload")
	}
	var p struct {
		Query         string         `json:"query"`
		Variables     map[string]any `json:"variables"`
		OperationName string         `json:"operationName"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil, fmt.Errorf("invalid operation payload")
	}

	// Same guard as the subscribe path (issue #317): reject an abusively long
	// query outright rather than running regexes across it.
	if len(p.Query) > gqlMaxQueryLen {
		return nil, fmt.Errorf("query exceeds maximum length of %d bytes", gqlMaxQueryLen)
	}
	if strings.TrimSpace(p.Query) == "" {
		return nil, fmt.Errorf("query is required")
	}

	name := ""
	if m := reOperationName.FindStringSubmatch(p.Query); len(m) == 2 {
		name = m[1]
	}
	// A leading `query`/`subscription` keyword is the document type, not the
	// root field, so step past it to the real first selection.
	if name == "query" || name == "subscription" {
		trimmed := p.Query[strings.Index(p.Query, "{")+1:]
		if m := reOperationName.FindStringSubmatch("{" + trimmed); len(m) == 2 {
			name = m[1]
		}
	}
	if name == "" {
		return nil, fmt.Errorf("could not determine operation")
	}

	vars := map[string]any{}
	// Inline arguments first, so explicit variables can override them.
	for _, m := range reStringArg.FindAllStringSubmatch(p.Query, -1) {
		vars[m[1]] = m[2]
	}
	for _, m := range reNumberArg.FindAllStringSubmatch(p.Query, -1) {
		if n, err := strconv.ParseInt(m[2], 10, 64); err == nil {
			vars[m[1]] = n
		}
	}
	for k, v := range p.Variables {
		vars[k] = v
	}

	return &gqlOperation{name: name, variables: vars, query: p.Query}, nil
}

// gqlString reads a string variable, tolerating the json.Number/float64 forms
// encoding/json produces for values that arrived as JSON numbers.
func gqlString(vars map[string]any, key string) string {
	v, ok := vars[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatInt(int64(t), 10)
	case int64:
		return strconv.FormatInt(t, 10)
	}
	return fmt.Sprint(v)
}

// gqlInt reads an integer variable. ok is false when the key is absent or the
// value is not numeric, which lets callers tell "not supplied" from "supplied
// as zero" — the distinction REST draws between an omitted ledger bound and
// an explicit 0.
func gqlInt(vars map[string]any, key string) (int64, bool) {
	v, ok := vars[key]
	if !ok || v == nil {
		return 0, false
	}
	switch t := v.(type) {
	case float64:
		return int64(t), true
	case int64:
		return t, true
	case int:
		return int64(t), true
	case string:
		n, err := strconv.ParseInt(t, 10, 64)
		return n, err == nil
	}
	return 0, false
}

// gqlError is a resolver failure carrying a canonical error code, so a
// GraphQL client sees the same taxonomy a REST client does.
type gqlError struct {
	code    httputil.ErrorCode
	message string
}

func (e *gqlError) Error() string { return e.message }

func gqlErrf(code httputil.ErrorCode, format string, args ...any) *gqlError {
	return &gqlError{code: code, message: fmt.Sprintf(format, args...)}
}

// NewBackendError lets an EventsBackend implementation report a failure with
// a canonical error code, so the code reaches the client's errors[].
// extensions.code instead of being flattened to INTERNAL. Exported because
// the backend lives in the handlers package, which is where the gRPC client
// and the database queries already are.
func NewBackendError(code httputil.ErrorCode, message string) error {
	return &gqlError{code: code, message: message}
}

// BackendErrorCode reports the canonical code carried by an error from
// NewBackendError, and whether it carried one at all. The concrete error type
// is unexported, so without this a caller outside this package — the backend
// implementation in handlers, or its tests — has no way to tell which
// classification a failure produced.
func BackendErrorCode(err error) (httputil.ErrorCode, bool) {
	var ge *gqlError
	if !errorsAs(err, &ge) {
		return "", false
	}
	return ge.code, true
}

// gqlResolveQuery dispatches one parsed operation against the backend and
// returns the `data` object for the reply.
//
// network is taken from the authenticated connection, never from the query,
// mirroring how the REST handlers derive it from the API key context: a
// caller cannot read another network's data by asking for it.
func gqlResolveQuery(ctx context.Context, backend EventsBackend, op *gqlOperation, network string) (map[string]any, error) {
	if backend == nil {
		return nil, gqlErrf(httputil.UNAVAILABLE, "events backend unavailable")
	}

	ctx, cancel := context.WithTimeout(ctx, gqlQueryTimeout)
	defer cancel()

	switch op.name {
	case "events":
		return gqlResolveEvents(ctx, backend, op, network)
	case "event":
		return gqlResolveEvent(ctx, backend, op, network)
	case "contractStats":
		return gqlResolveContractStats(ctx, backend, op, network)
	default:
		return nil, gqlErrf(httputil.INVALID_ARGUMENT,
			"unsupported operation %q; supported operations are events, event, contractStats and the contractEvents subscription", op.name)
	}
}

// gqlResolveEvents implements the events query at REST parity: the same
// filters, the same limit ceiling, and the same opaque-cursor pagination as
// GET /v1/events.
func gqlResolveEvents(ctx context.Context, backend EventsBackend, op *gqlOperation, network string) (map[string]any, error) {
	req := EventsQuery{
		ContractID: gqlString(op.variables, "contractId"),
		Topic0:     gqlString(op.variables, "topic0"),
		Topic1:     gqlString(op.variables, "topic1"),
		EventType:  gqlString(op.variables, "eventType"),
		Cursor:     gqlString(op.variables, "cursor"),
		Network:    network,
		Limit:      gqlDefaultPageSize,
	}

	if n, ok := gqlInt(op.variables, "limit"); ok {
		if n < 1 || n > gqlMaxPageSize {
			return nil, gqlErrf(httputil.INVALID_ARGUMENT,
				"limit must be an integer between 1 and %d", gqlMaxPageSize)
		}
		req.Limit = int(n)
	}
	if n, ok := gqlInt(op.variables, "ledgerFrom"); ok {
		if n < 0 {
			return nil, gqlErrf(httputil.INVALID_ARGUMENT, "ledgerFrom must not be negative")
		}
		req.LedgerFrom = &n
	}
	if n, ok := gqlInt(op.variables, "ledgerTo"); ok {
		if n < 0 {
			return nil, gqlErrf(httputil.INVALID_ARGUMENT, "ledgerTo must not be negative")
		}
		req.LedgerTo = &n
	}
	if req.LedgerFrom != nil && req.LedgerTo != nil && *req.LedgerTo < *req.LedgerFrom {
		return nil, gqlErrf(httputil.INVALID_ARGUMENT, "ledgerTo must be greater than or equal to ledgerFrom")
	}

	page, err := backend.ListEvents(ctx, req)
	if err != nil {
		return nil, gqlBackendError(err)
	}

	events := make([]any, 0, len(page.Events))
	for _, e := range page.Events {
		events = append(events, gqlEventFields(e))
	}

	// nextCursor is emitted as an explicit null on the last page rather than
	// omitted, matching the REST envelope (issue #575) so one auto-pager rule
	// — stop when nextCursor is null — works on both transports.
	var next any
	if page.NextCursor != nil {
		next = *page.NextCursor
	}

	return map[string]any{
		"events": map[string]any{
			"events":     events,
			"hasMore":    page.HasMore,
			"nextCursor": next,
		},
	}, nil
}

// gqlResolveEvent implements the single-event query, the GraphQL counterpart
// of GET /v1/events/{id}.
func gqlResolveEvent(ctx context.Context, backend EventsBackend, op *gqlOperation, network string) (map[string]any, error) {
	id := gqlString(op.variables, "id")
	if id == "" {
		return nil, gqlErrf(httputil.INVALID_ARGUMENT, "id is required")
	}

	ev, err := backend.GetEvent(ctx, id, network)
	if err != nil {
		return nil, gqlBackendError(err)
	}
	if ev == nil {
		// GraphQL convention is a null field rather than a transport error
		// for "no such object"; the canonical NOT_FOUND code still travels in
		// the errors array when the backend reports one.
		return map[string]any{"event": nil}, nil
	}
	return map[string]any{"event": gqlEventFields(ev)}, nil
}

// gqlResolveContractStats implements the stats query, the GraphQL counterpart
// of GET /v1/stats/contracts.
func gqlResolveContractStats(ctx context.Context, backend EventsBackend, op *gqlOperation, network string) (map[string]any, error) {
	req := StatsQuery{Network: network, Limit: gqlDefaultPageSize}

	if n, ok := gqlInt(op.variables, "limit"); ok {
		if n < 1 || n > gqlMaxPageSize {
			return nil, gqlErrf(httputil.INVALID_ARGUMENT,
				"limit must be an integer between 1 and %d", gqlMaxPageSize)
		}
		req.Limit = int(n)
	}
	if n, ok := gqlInt(op.variables, "fromLedger"); ok {
		if n < 0 {
			return nil, gqlErrf(httputil.INVALID_ARGUMENT, "fromLedger must not be negative")
		}
		req.FromLedger = n
	}
	if n, ok := gqlInt(op.variables, "toLedger"); ok {
		if n < 0 {
			return nil, gqlErrf(httputil.INVALID_ARGUMENT, "toLedger must not be negative")
		}
		req.ToLedger = n
	}
	if req.ToLedger != 0 && req.ToLedger < req.FromLedger {
		return nil, gqlErrf(httputil.INVALID_ARGUMENT, "toLedger must be greater than or equal to fromLedger")
	}

	rows, err := backend.ContractStats(ctx, req)
	if err != nil {
		return nil, gqlBackendError(err)
	}

	stats := make([]any, 0, len(rows))
	for _, row := range rows {
		stats = append(stats, gqlStatsFields(row))
	}
	return map[string]any{"contractStats": stats}, nil
}

// gqlBackendError maps a backend failure onto the canonical taxonomy. A
// resolver error that already carries a code passes through unchanged;
// anything else is INTERNAL, so no backend detail leaks into a client-facing
// message.
func gqlBackendError(err error) error {
	var ge *gqlError
	if errorsAs(err, &ge) {
		return ge
	}
	return gqlErrf(httputil.INTERNAL, "failed to resolve query")
}

// gqlEventFields remaps a snake_case event row to the camelCase GraphQL
// schema field names — the same mapping gqlNextMsg applies to subscription
// payloads, so a field means the same thing however it was delivered.
func gqlEventFields(src map[string]any) map[string]any {
	ev := make(map[string]any, 10)
	pick := func(dst, key string) {
		if v, ok := src[key]; ok {
			ev[dst] = v
		}
	}
	pick("id", "id")
	pick("contractId", "contract_id")
	pick("ledgerSequence", "ledger_sequence")
	pick("ledgerTimestamp", "ledger_timestamp")
	pick("txHash", "transaction_hash")
	pick("eventIndex", "event_index")
	pick("eventType", "event_type")
	pick("topics", "topics")
	pick("data", "data")
	pick("createdAt", "created_at")
	return ev
}

// gqlStatsFields remaps a stats row to camelCase GraphQL field names.
func gqlStatsFields(src map[string]any) map[string]any {
	out := make(map[string]any, 6)
	pick := func(dst, key string) {
		if v, ok := src[key]; ok {
			out[dst] = v
		}
	}
	pick("contractId", "contract_id")
	pick("eventCount", "event_count")
	pick("firstLedger", "first_ledger")
	pick("lastLedger", "last_ledger")
	pick("firstSeen", "first_seen")
	pick("lastSeen", "last_seen")
	return out
}

// gqlDataMsg builds a graphql-transport-ws `next` envelope for a query
// result. A query completes after one next + complete, unlike a subscription
// which streams many next messages.
func gqlDataMsg(opID string, data map[string]any) gqlMessage {
	inner, err := json.Marshal(data)
	if err != nil {
		inner = []byte(`null`)
	}
	return gqlMessage{
		Type:    "next",
		ID:      opID,
		Payload: json.RawMessage(`{"data":` + string(inner) + `}`),
	}
}

// gqlCodedErrorMsg builds an error message carrying a canonical error code in
// extensions.code, which is where the GraphQL spec puts machine-readable
// error classification and where the SDKs look for it.
// An error that carries no canonical code is one this package did not
// classify — most likely straight from a backend driver. Its text is not
// safe to forward: a pgx/pq error names internal tables and columns, which
// is exactly the detail the REST handlers are careful to replace with a
// generic message before writing a 500. So an unclassified error becomes a
// bare INTERNAL, and the real text is left for the server logs.
func gqlCodedErrorMsg(id string, err error) gqlMessage {
	code := httputil.INTERNAL
	message := "internal error"

	var ge *gqlError
	if errorsAs(err, &ge) {
		code = ge.code
		message = ge.message
	}

	errs, _ := json.Marshal([]map[string]any{{
		"message":    message,
		"extensions": map[string]any{"code": string(code)},
	}})
	return gqlMessage{Type: "error", ID: id, Payload: json.RawMessage(errs)}
}

// errorsAs is errors.As, wrapped so this file does not import errors purely
// for one call and so the target type stays explicit at each call site.
func errorsAs(err error, target **gqlError) bool {
	if err == nil {
		return false
	}
	if ge, ok := err.(*gqlError); ok {
		*target = ge
		return true
	}
	return false
}

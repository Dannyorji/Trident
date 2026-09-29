package handlers

import (
	"context"
	"net/http"

	"github.com/Depo-dev/trident/services/api/cursor"
	"github.com/Depo-dev/trident/services/api/gen"
	"github.com/Depo-dev/trident/services/api/internal/httputil"
	"github.com/Depo-dev/trident/services/api/validation"
	"github.com/Depo-dev/trident/services/api/ws"
)

// GraphQLBackend resolves the GraphQL query operations (issue #223) against
// the same backends the REST handlers use: the shared gRPC events client for
// events, and the same database aggregate for contract stats.
//
// Sharing the backends rather than reimplementing them is what makes parity
// structural. A filter, a validation rule, or a fix to the stats aggregation
// lands on both transports at once, instead of the two drifting apart the way
// partial GraphQL coverage did before.
type GraphQLBackend struct {
	// DB backs the contractStats operation. A nil DB makes that operation
	// report UNAVAILABLE while the event operations keep working.
	DB DBPool
}

// NewGraphQLBackend builds the backend the GraphQL transport resolves through.
func NewGraphQLBackend(db DBPool) *GraphQLBackend {
	return &GraphQLBackend{DB: db}
}

// ListEvents resolves the `events` query through the same gRPC client and the
// same opaque-cursor encoding GET /v1/events uses, so both transports page
// identically and a cursor from one is meaningful to the other.
func (b *GraphQLBackend) ListEvents(ctx context.Context, req ws.EventsQuery) (ws.EventsPage, error) {
	if eventsClient == nil {
		return ws.EventsPage{}, ws.NewBackendError(httputil.UNAVAILABLE, "gRPC backend unavailable")
	}

	// The cursor arrives opaque and must be decoded to the internal paging
	// token, exactly as ListEvents does — an undecoded cursor would be
	// silently wrong rather than loudly rejected.
	var pagingToken string
	if req.Cursor != "" {
		decoded, verr := validation.ValidateCursor("cursor", req.Cursor)
		if verr != nil {
			return ws.EventsPage{}, ws.NewBackendError(httputil.INVALID_ARGUMENT, verr.Message)
		}
		pagingToken = decoded
	}

	if req.ContractID != "" {
		if verr := validation.ValidateContractID("contractId", req.ContractID); verr != nil {
			return ws.EventsPage{}, ws.NewBackendError(httputil.INVALID_ARGUMENT, verr.Message)
		}
	}

	grpcReq := &gen.ListEventsRequest{
		ContractId: req.ContractID,
		Topic_0:    req.Topic0,
		Topic_1:    req.Topic1,
		Cursor:     pagingToken,
		Limit:      uint32(req.Limit),
		Network:    req.Network,
	}
	if req.LedgerFrom != nil {
		grpcReq.LedgerFrom = uint64(*req.LedgerFrom)
	}
	if req.LedgerTo != nil {
		grpcReq.LedgerTo = uint64(*req.LedgerTo)
	}

	resp, err := eventsClient.ListEvents(ctx, grpcReq)
	if err != nil {
		return ws.EventsPage{}, grpcToBackendError(err, "failed to fetch events")
	}

	events := make([]map[string]any, 0, len(resp.Events))
	for _, e := range resp.Events {
		events = append(events, eventToMap(protoEventToJSON(e)))
	}

	page := ws.EventsPage{Events: events, HasMore: resp.HasMore}
	if resp.NextCursor != "" {
		encoded := cursor.Encode(resp.NextCursor)
		page.NextCursor = &encoded
	}
	return page, nil
}

// GetEvent resolves the `event` query, the counterpart of GET /v1/events/{id}.
func (b *GraphQLBackend) GetEvent(ctx context.Context, id, network string) (map[string]any, error) {
	if verr := validation.ValidateEventID(id); verr != nil {
		return nil, ws.NewBackendError(httputil.INVALID_ARGUMENT, verr.Message)
	}
	if eventsClient == nil {
		return nil, ws.NewBackendError(httputil.UNAVAILABLE, "gRPC backend unavailable")
	}

	event, err := eventsClient.GetEvent(ctx, &gen.GetEventRequest{Id: id, Network: network})
	if err != nil {
		// As in GetEvent: only a real 404 may present as "not found". A
		// timeout or backend outage must not masquerade as a missing event
		// (issue #227).
		statusCode, code := httputil.GRPCToHTTP(err)
		if statusCode == http.StatusNotFound {
			return nil, nil
		}
		return nil, ws.NewBackendError(code, "failed to fetch event")
	}
	return eventToMap(protoEventToJSON(event)), nil
}

// ContractStats resolves the `contractStats` query through the same aggregate
// GET /v1/stats/contracts runs.
func (b *GraphQLBackend) ContractStats(ctx context.Context, req ws.StatsQuery) ([]map[string]any, error) {
	if b.DB == nil {
		return nil, ws.NewBackendError(httputil.UNAVAILABLE, "database unavailable")
	}

	params := &validation.QueryStatsParams{
		FromLedger: req.FromLedger,
		ToLedger:   req.ToLedger,
		Network:    req.Network,
		Limit:      int64(req.Limit),
	}
	if req.FromLedger > 0 {
		from := req.FromLedger
		params.FromLedgerPtr = &from
	}
	if req.ToLedger > 0 {
		to := req.ToLedger
		params.ToLedgerPtr = &to
	}

	// GraphQL contractStats exposes no cursor argument, so this is always the
	// first page: nil keyset. queryContractStats fetches limit+1 rows to let
	// REST callers detect a next page, so trim the probe row here.
	stats, err := queryContractStats(ctx, b.DB, params, nil)
	if err != nil {
		return nil, ws.NewBackendError(httputil.INTERNAL, "failed to fetch statistics")
	}
	if len(stats) > int(params.Limit) {
		stats = stats[:params.Limit]
	}

	out := make([]map[string]any, 0, len(stats))
	for _, s := range stats {
		out = append(out, map[string]any{
			"contract_id":  s.ContractID,
			"event_count":  s.EventCount,
			"last_ledger":  s.LastSeenLedger,
			"last_seen":    s.LastSeenAt,
			"first_ledger": nil,
			"first_seen":   nil,
		})
	}
	return out, nil
}

// eventToMap converts an EventJSON to the snake_case map the GraphQL field
// mapper expects. It goes through the same EventJSON the REST response is
// built from, so the two transports cannot disagree about a field's value.
func eventToMap(e *EventJSON) map[string]any {
	if e == nil {
		return nil
	}
	return map[string]any{
		"id":               e.ID,
		"contract_id":      e.ContractID,
		"ledger_sequence":  e.LedgerSequence,
		"ledger_timestamp": e.LedgerTimestamp,
		"transaction_hash": e.TransactionHash,
		"event_index":      e.EventIndex,
		"event_type":       e.EventType,
		"topics":           e.Topics,
		"data":             e.Data,
		"created_at":       e.CreatedAt,
	}
}

// grpcToBackendError maps a gRPC failure onto the canonical error taxonomy,
// reusing the same GRPCToHTTP mapping the REST handlers use so a given
// backend failure produces the same code on both transports.
func grpcToBackendError(err error, message string) error {
	_, code := httputil.GRPCToHTTP(err)
	return ws.NewBackendError(code, message)
}

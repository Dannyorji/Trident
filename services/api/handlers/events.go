package handlers

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/Depo-dev/trident/services/api/cursor"
	"github.com/Depo-dev/trident/services/api/gen"
	"github.com/Depo-dev/trident/services/api/internal/httputil"
	"github.com/Depo-dev/trident/services/api/middleware"
	"github.com/Depo-dev/trident/services/api/validation"
)

// grpcCallTimeout is the per-call deadline applied to all gRPC backend requests.
const grpcCallTimeout = 10 * time.Second

// ListEventsResponse is the response envelope for GET /v1/events.
type ListEventsResponse struct {
	Events     []*EventJSON `json:"events"`
	HasMore    bool         `json:"has_more"`
	NextCursor *string      `json:"next_cursor"`
}

// EventJSON is the JSON representation of an event
type EventJSON struct {
	ID              string   `json:"id"`
	ContractID      string   `json:"contract_id"`
	LedgerSequence  uint64   `json:"ledger_sequence"`
	LedgerTimestamp string   `json:"ledger_timestamp"`
	TransactionHash string   `json:"transaction_hash"`
	EventIndex      uint32   `json:"event_index"`
	EventType       string   `json:"event_type"`
	Topics          []string `json:"topics"`
	Data            string   `json:"data"`
	CreatedAt       string   `json:"created_at"`
}

// eventsClient is set at initialization
var eventsClient gen.EventsClient

// SetEventsClient sets the gRPC events client (called from main)
func SetEventsClient(client gen.EventsClient) {
	eventsClient = client
}

// ListEvents handles GET /v1/events (issues #42, #44, #143).
//
// Validates query parameters and decodes the opaque pagination cursor before
// forwarding to the gRPC backend. The network is derived from the authenticated
// API key context and enforced server-side — callers cannot override it.
// Returns 400 on any validation failure.
func ListEvents(w http.ResponseWriter, r *http.Request) {
	// Input is validated before backend availability: a malformed request is
	// invalid whether or not the gRPC backend is up, and answering 503 for it
	// tells the client to retry something that can never succeed (#222).
	q := r.URL.Query()
	// An unrecognised parameter is a client bug — a typo'd `limitt` silently
	// changing the page size hides it — so it is rejected, not ignored (#222).
	if verr := validation.RejectUnknownParams(
		q, "limit", "ledgerFrom", "ledgerTo", "contractId", "cursor", "event_type", "topic0", "topic1",
	); verr != nil {
		httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, verr.Message)
		return
	}

	params, verr := validation.ValidateQueryEvents(
		q.Get("limit"),
		q.Get("ledgerFrom"),
		q.Get("ledgerTo"),
		q.Get("contractId"),
		q.Get("cursor"),
		q.Get("event_type"),
	)
	if verr != nil {
		httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, verr.Message)
		return
	}

	// Decode opaque cursor → internal paging token (issues #44, #222).
	pagingToken, verr := validation.ValidateCursor("cursor", q.Get("cursor"))
	if verr != nil {
		httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, verr.Message)
		return
	}

	if eventsClient == nil {
		httputil.WriteErrorCtx(r.Context(), w, http.StatusServiceUnavailable, httputil.INTERNAL, "gRPC backend unavailable")
		return
	}

	// Network is enforced server-side from the authenticated API key context.
	// Unauthenticated requests default to "testnet".
	network := middleware.NetworkFromContext(r.Context())

	grpcReq := &gen.ListEventsRequest{
		ContractId: params.ContractID,
		Topic_0:    q.Get("topic0"),
		Topic_1:    q.Get("topic1"),
		Cursor:     pagingToken,
		Limit:      uint32(params.Limit),
		Network:    network,
	}

	if params.LedgerFrom != nil {
		grpcReq.LedgerFrom = uint64(*params.LedgerFrom)
	}
	if params.LedgerTo != nil {
		grpcReq.LedgerTo = uint64(*params.LedgerTo)
	}

	ctx, cancel := context.WithTimeout(r.Context(), grpcCallTimeout)
	defer cancel()

	resp, err := eventsClient.ListEvents(ctx, grpcReq)
	if err != nil {
		statusCode, code := httputil.GRPCToHTTP(err)
		slog.ErrorContext(r.Context(), "grpc ListEvents failed", "err", err)
		httputil.WriteErrorCtx(r.Context(), w, statusCode, code, "failed to fetch events")
		return
	}

	events := make([]*EventJSON, len(resp.Events))
	for i, event := range resp.Events {
		events[i] = protoEventToJSON(event)
	}

	hasMore := resp.HasMore
	var nextCursor *string
	if resp.NextCursor != "" {
		encoded := cursor.Encode(resp.NextCursor)
		nextCursor = &encoded
	}

	writeJSON(w, http.StatusOK, ListEventsResponse{
		Events:     events,
		HasMore:    hasMore,
		NextCursor: nextCursor,
	})
}

// GetEvent handles GET /v1/events/{id}.
//
// Validates the :id path parameter as a UUID v4 (issue #42). The network is
// derived from the authenticated API key context to prevent cross-network
// data exposure. Returns 400 when the format is invalid.
func GetEvent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if verr := validation.ValidateEventID(id); verr != nil {
		httputil.WriteErrorCtx(r.Context(), w, http.StatusBadRequest, httputil.INVALID_ARGUMENT, verr.Message)
		return
	}

	if eventsClient == nil {
		httputil.WriteErrorCtx(r.Context(), w, http.StatusServiceUnavailable, httputil.INTERNAL, "gRPC backend unavailable")
		return
	}

	// Network enforced from authenticated API key context.
	network := middleware.NetworkFromContext(r.Context())

	ctx, cancel := context.WithTimeout(r.Context(), grpcCallTimeout)
	defer cancel()

	event, err := eventsClient.GetEvent(ctx, &gen.GetEventRequest{Id: id, Network: network})
	if err != nil {
		statusCode, code := httputil.GRPCToHTTP(err)
		slog.ErrorContext(r.Context(), "grpc GetEvent failed", "err", err)
		// "event not found" only fits a 404; a timeout or backend outage
		// must not masquerade as a missing event (issue #227).
		msg := "event not found"
		if statusCode != http.StatusNotFound {
			msg = "failed to fetch event"
		}
		httputil.WriteErrorCtx(r.Context(), w, statusCode, code, msg)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"event": protoEventToJSON(event),
	})
}

// protoEventToJSON converts a proto Event to EventJSON
func protoEventToJSON(event *gen.Event) *EventJSON {
	return &EventJSON{
		ID:              event.Id,
		ContractID:      event.ContractId,
		LedgerSequence:  event.LedgerSequence,
		LedgerTimestamp: event.LedgerTimestamp,
		TransactionHash: event.TransactionHash,
		EventIndex:      event.EventIndex,
		EventType:       event.EventType,
		Topics:          event.Topics,
		Data:            event.Data,
		CreatedAt:       event.CreatedAt,
	}
}

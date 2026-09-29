package handlers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Depo-dev/trident/services/api/cursor"
	"github.com/Depo-dev/trident/services/api/gen"
	"github.com/Depo-dev/trident/services/api/handlers"
)

// TestRESTGraphQLParity_EventFields verifies that the REST event response
// includes every field defined in the documented GraphQL Event type
// (docs/site/graphql/schema.mdx). When GraphQL query support lands, these
// same fields must be selectable via the GraphQL interface and return
// identical values for the same event.
func TestRESTGraphQLParity_EventFields(t *testing.T) {
	mock := &MockEventsClient{
		ListEventsFunc: func(ctx context.Context, req *gen.ListEventsRequest) (*gen.ListEventsResponse, error) {
			return &gen.ListEventsResponse{
				Events: []*gen.Event{
					{
						Id:              "550e8400-e29b-41d4-a716-446655440000",
						ContractId:      "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAD2KM",
						LedgerSequence:  12345,
						LedgerTimestamp: "2026-01-15T10:30:00Z",
						TransactionHash: "abc123def456",
						EventIndex:      0,
						EventType:       "contract",
						Topics:          []string{"AAAADQAAAAh0cmFuc2Zlcg=="},
						Data:            "AAAA",
						CreatedAt:       "2026-01-15T10:30:01Z",
					},
				},
				HasMore:    false,
				NextCursor: "",
			}, nil
		},
	}
	handlers.SetEventsClient(mock)

	req := httptest.NewRequest(http.MethodGet, "/v1/events?limit=1", nil)
	rr := httptest.NewRecorder()
	handlers.ListEvents(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}

	var body map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	events, ok := body["events"].([]any)
	if !ok || len(events) == 0 {
		t.Fatal("expected at least one event")
	}

	event := events[0].(map[string]any)

	// Fields that must exist in both REST and GraphQL responses.
	// These match the documented GraphQL Event type in docs/site/graphql/schema.mdx.
	requiredFields := []string{
		"id",
		"contract_id",
		"ledger_sequence",
		"ledger_timestamp",
		"transaction_hash",
		"event_index",
		"event_type",
		"topics",
		"data",
		"created_at",
	}

	for _, field := range requiredFields {
		if _, ok := event[field]; !ok {
			t.Errorf("REST event missing field %q that GraphQL Event type defines", field)
		}
	}

	// Pagination envelope must also match the GraphQL PageInfo shape.
	if _, ok := body["has_more"]; !ok {
		t.Error("response missing has_more (GraphQL: pageInfo.hasNextPage)")
	}
	if _, ok := body["next_cursor"]; !ok {
		t.Error("response missing next_cursor (GraphQL: pageInfo.endCursor)")
	}
}

// TestRESTGraphQLParity_PaginationCursorRoundTrip verifies that a cursor
// returned by one REST response can be passed back in the next request and
// produces a valid result — the same contract GraphQL connections enforce
// via Relay-style cursor pagination.
func TestRESTGraphQLParity_PaginationCursorRoundTrip(t *testing.T) {
	callCount := 0
	mock := &MockEventsClient{
		ListEventsFunc: func(ctx context.Context, req *gen.ListEventsRequest) (*gen.ListEventsResponse, error) {
			callCount++
			if callCount == 1 {
				return &gen.ListEventsResponse{
					Events: []*gen.Event{
						{Id: "00000000-0000-0000-0000-000000000001", ContractId: "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAD2KM", LedgerSequence: 1},
					},
					HasMore:    true,
					NextCursor: "ledger:1",
				}, nil
			}
			return &gen.ListEventsResponse{
				Events: []*gen.Event{
					{Id: "00000000-0000-0000-0000-000000000002", ContractId: "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAD2KM", LedgerSequence: 2},
				},
				HasMore:    false,
				NextCursor: "",
			}, nil
		},
	}
	handlers.SetEventsClient(mock)

	// First page.
	req1 := httptest.NewRequest(http.MethodGet, "/v1/events?limit=1", nil)
	rr1 := httptest.NewRecorder()
	handlers.ListEvents(rr1, req1)

	if rr1.Code != http.StatusOK {
		t.Fatalf("page 1: want 200, got %d", rr1.Code)
	}

	var body1 map[string]any
	if err := json.NewDecoder(rr1.Body).Decode(&body1); err != nil {
		t.Fatalf("page 1: decode: %v", err)
	}

	nextCursor, ok := body1["next_cursor"].(string)
	if !ok || nextCursor == "" {
		t.Fatal("expected non-empty next_cursor")
	}

	// Decode then re-encode to simulate what a real client does.
	decoded, verr := cursor.Decode(nextCursor)
	if verr != nil {
		t.Fatalf("cursor decode failed: %v", verr)
	}
	reshuffled := cursor.Encode(decoded)

	// Second page using the cursor.
	req2 := httptest.NewRequest(http.MethodGet, "/v1/events?limit=1&cursor="+reshuffled, nil)
	rr2 := httptest.NewRecorder()
	handlers.ListEvents(rr2, req2)

	if rr2.Code != http.StatusOK {
		t.Fatalf("page 2: want 200, got %d", rr2.Code)
	}

	var body2 map[string]any
	if err := json.NewDecoder(rr2.Body).Decode(&body2); err != nil {
		t.Fatalf("page 2: decode: %v", err)
	}

	if body2["has_more"] != false {
		t.Error("page 2 should have has_more=false")
	}
	if body2["next_cursor"] != nil {
		t.Error("page 2 should have null next_cursor")
	}

	events2 := body2["events"].([]any)
	if len(events2) != 1 {
		t.Fatalf("page 2: want 1 event, got %d", len(events2))
	}
}

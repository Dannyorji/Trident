package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Depo-dev/trident/services/api/gen"
	"github.com/Depo-dev/trident/services/api/handlers"
	"github.com/Depo-dev/trident/services/api/middleware"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	uuid1 = "550e8400-e29b-41d4-a716-446655440001"
	uuid2 = "550e8400-e29b-41d4-a716-446655440002"
	uuid3 = "550e8400-e29b-41d4-a716-446655440003"
)

func fakeEvent(id string) *gen.Event {
	return &gen.Event{
		Id:              id,
		ContractId:      "CABC123",
		LedgerSequence:  42,
		LedgerTimestamp: "2026-01-01T00:00:00Z",
		TransactionHash: "deadbeef",
		EventIndex:      0,
		EventType:       "contract",
		Topics:          []string{"transfer"},
		Data:            `{}`,
		CreatedAt:       "2026-01-01T00:00:01Z",
	}
}

func batchPost(body any) *http.Request {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/v1/events/batch", bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestBatchGetEvents_AllFound(t *testing.T) {
	handlers.SetEventsClient(&MockEventsClient{
		GetEventFunc: func(_ context.Context, req *gen.GetEventRequest) (*gen.Event, error) {
			return fakeEvent(req.Id), nil
		},
	})

	w := httptest.NewRecorder()
	handlers.BatchGetEvents(w, batchPost(map[string]any{"ids": []string{uuid1, uuid2}}))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp handlers.BatchEventsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Events) != 2 {
		t.Errorf("expected 2 events, got %d", len(resp.Events))
	}
	if len(resp.Missing) != 0 {
		t.Errorf("expected 0 missing, got %v", resp.Missing)
	}
}

func TestBatchGetEvents_SomeMissing(t *testing.T) {
	handlers.SetEventsClient(&MockEventsClient{
		GetEventFunc: func(_ context.Context, req *gen.GetEventRequest) (*gen.Event, error) {
			if req.Id == uuid1 {
				return fakeEvent(uuid1), nil
			}
			return nil, status.Error(codes.NotFound, "not found")
		},
	})

	w := httptest.NewRecorder()
	handlers.BatchGetEvents(w, batchPost(map[string]any{"ids": []string{uuid1, uuid2}}))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp handlers.BatchEventsResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Events) != 1 {
		t.Errorf("expected 1 event, got %d", len(resp.Events))
	}
	if len(resp.Missing) != 1 || resp.Missing[0] != uuid2 {
		t.Errorf("expected [%s] in missing, got %v", uuid2, resp.Missing)
	}
}

func TestBatchGetEvents_LimitExceeded(t *testing.T) {
	handlers.SetEventsClient(&MockEventsClient{})

	ids := make([]string, 101)
	for i := range ids {
		ids[i] = uuid1
	}
	w := httptest.NewRecorder()
	handlers.BatchGetEvents(w, batchPost(map[string]any{"ids": ids}))

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "100") {
		t.Errorf("expected limit message, got: %s", w.Body.String())
	}
}

func TestBatchGetEvents_InvalidUUID(t *testing.T) {
	handlers.SetEventsClient(&MockEventsClient{})

	w := httptest.NewRecorder()
	handlers.BatchGetEvents(w, batchPost(map[string]any{"ids": []string{"not-a-uuid", uuid1}}))

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
	var body map[string]any
	_ = json.NewDecoder(w.Body).Decode(&body)
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "INVALID_ARGUMENT" {
		t.Errorf("expected error.code=INVALID_ARGUMENT, got: %v", body)
	}
	if msg, _ := errObj["message"].(string); !strings.Contains(msg, "not-a-uuid") {
		t.Errorf("expected message to list the invalid id, got: %v", body)
	}
}

func TestBatchGetEvents_InvalidJSON(t *testing.T) {
	handlers.SetEventsClient(&MockEventsClient{})

	r := httptest.NewRequest(http.MethodPost, "/v1/events/batch", strings.NewReader("{bad json"))
	w := httptest.NewRecorder()
	handlers.BatchGetEvents(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestBatchGetEvents_EmptyMissingIsArray(t *testing.T) {
	handlers.SetEventsClient(&MockEventsClient{
		GetEventFunc: func(_ context.Context, req *gen.GetEventRequest) (*gen.Event, error) {
			return fakeEvent(req.Id), nil
		},
	})

	w := httptest.NewRecorder()
	handlers.BatchGetEvents(w, batchPost(map[string]any{"ids": []string{uuid1}}))

	body := w.Body.String()
	if !strings.Contains(body, `"missing":[]`) {
		t.Errorf("missing should be empty array, got: %s", body)
	}
}

func TestBatchGetEvents_EmptyIDs(t *testing.T) {
	handlers.SetEventsClient(&MockEventsClient{})

	w := httptest.NewRecorder()
	handlers.BatchGetEvents(w, batchPost(map[string]any{"ids": []string{}}))

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
	var body map[string]any
	_ = json.NewDecoder(w.Body).Decode(&body)
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "INVALID_ARGUMENT" {
		t.Errorf("expected INVALID_ARGUMENT, got: %v", body)
	}
}

func TestBatchGetEvents_PreservesRequestOrder(t *testing.T) {
	handlers.SetEventsClient(&MockEventsClient{
		GetEventFunc: func(_ context.Context, req *gen.GetEventRequest) (*gen.Event, error) {
			return fakeEvent(req.Id), nil
		},
	})

	w := httptest.NewRecorder()
	handlers.BatchGetEvents(w, batchPost(map[string]any{"ids": []string{uuid3, uuid1, uuid2}}))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp handlers.BatchEventsResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	want := []string{uuid3, uuid1, uuid2}
	if len(resp.Events) != len(want) {
		t.Fatalf("expected %d events, got %d", len(want), len(resp.Events))
	}
	for i, id := range want {
		if resp.Events[i].ID != id {
			t.Errorf("events[%d] = %s, want %s (request order must be preserved)", i, resp.Events[i].ID, id)
		}
	}
}

func TestBatchGetEvents_MissingPreservesRequestOrder(t *testing.T) {
	handlers.SetEventsClient(&MockEventsClient{
		GetEventFunc: func(_ context.Context, _ *gen.GetEventRequest) (*gen.Event, error) {
			return nil, status.Error(codes.NotFound, "not found")
		},
	})

	w := httptest.NewRecorder()
	handlers.BatchGetEvents(w, batchPost(map[string]any{"ids": []string{uuid2, uuid3, uuid1}}))

	var resp handlers.BatchEventsResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	want := []string{uuid2, uuid3, uuid1}
	if len(resp.Missing) != len(want) {
		t.Fatalf("expected %d missing, got %v", len(want), resp.Missing)
	}
	for i, id := range want {
		if resp.Missing[i] != id {
			t.Errorf("missing[%d] = %s, want %s (request order must be preserved)", i, resp.Missing[i], id)
		}
	}
}

func TestBatchGetEvents_DuplicateIDsDeduplicated(t *testing.T) {
	var calls atomic.Int64
	handlers.SetEventsClient(&MockEventsClient{
		GetEventFunc: func(_ context.Context, req *gen.GetEventRequest) (*gen.Event, error) {
			calls.Add(1)
			return fakeEvent(req.Id), nil
		},
	})

	w := httptest.NewRecorder()
	handlers.BatchGetEvents(w, batchPost(map[string]any{"ids": []string{uuid1, uuid2, uuid1, uuid1}}))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp handlers.BatchEventsResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Events) != 2 {
		t.Errorf("expected 2 deduplicated events, got %d", len(resp.Events))
	}
	if len(resp.Events) == 2 && (resp.Events[0].ID != uuid1 || resp.Events[1].ID != uuid2) {
		t.Errorf("expected first-occurrence order [%s %s], got [%s %s]", uuid1, uuid2, resp.Events[0].ID, resp.Events[1].ID)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("expected 2 backend calls after dedupe, got %d", n)
	}
}

// TestBatchGetEvents_ScopesToAuthenticatedNetwork guards against issue #613:
// the batch route must forward Network on every gRPC GetEvent call, exactly
// like the single-event route (events.go:170), so a mainnet-only event id
// cannot be read by batching it through a testnet-scoped key. Before the
// fix, GetEventRequest{Id: id} omitted Network entirely, so the backend
// received an empty/default network and a single id batched through this
// route bypassed the scope GET /v1/events/{id} enforces.
func TestBatchGetEvents_ScopesToAuthenticatedNetwork(t *testing.T) {
	var gotNetwork string
	handlers.SetEventsClient(&MockEventsClient{
		GetEventFunc: func(_ context.Context, req *gen.GetEventRequest) (*gen.Event, error) {
			gotNetwork = req.Network
			// Simulates the real backend's per-network scoping: the event
			// only "exists" on mainnet, so a request scoped to any other
			// network must report it missing, never leak it.
			if req.Network != "mainnet" {
				return nil, status.Error(codes.NotFound, "not found")
			}
			return fakeEvent(req.Id), nil
		},
	})

	req := batchPost(map[string]any{"ids": []string{uuid1}})
	req = req.WithContext(middleware.WithNetwork(req.Context(), "testnet"))
	w := httptest.NewRecorder()
	handlers.BatchGetEvents(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotNetwork != "testnet" {
		t.Fatalf("expected batch handler to forward the authenticated network %q to the backend, got %q", "testnet", gotNetwork)
	}
	var resp handlers.BatchEventsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Events) != 0 {
		t.Errorf("expected the mainnet-only event to be reported missing for a testnet-scoped request, got %d events", len(resp.Events))
	}
	if len(resp.Missing) != 1 || resp.Missing[0] != uuid1 {
		t.Errorf("expected [%s] in missing, got %v", uuid1, resp.Missing)
	}
}

func TestBatchGetEvents_OverLimitCountsDuplicates(t *testing.T) {
	handlers.SetEventsClient(&MockEventsClient{})

	// 101 copies of the same id: the limit applies before deduplication.
	ids := make([]string, 101)
	for i := range ids {
		ids[i] = uuid1
	}
	w := httptest.NewRecorder()
	handlers.BatchGetEvents(w, batchPost(map[string]any{"ids": ids}))

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

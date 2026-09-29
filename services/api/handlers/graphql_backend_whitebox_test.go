package handlers

import (
	"context"
	"errors"
	"testing"

	"github.com/Depo-dev/trident/services/api/cursor"
	"github.com/Depo-dev/trident/services/api/gen"
	"github.com/Depo-dev/trident/services/api/internal/httputil"
	"github.com/Depo-dev/trident/services/api/ws"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Tests for the GraphQL query backend (issue #223).
//
// The point of this backend is that GraphQL resolves through the same gRPC
// client, the same cursor encoding and the same validation the REST handlers
// use. So these tests cover the pass-through and the error mapping, which is
// where a drift between the two transports would actually show up.

// gqlFakeEventsClient is a full gen.EventsClient double. health_test.go's
// fakeEventsClient only implements ListEvents, so it cannot stand in for the
// package-level eventsClient that GetEvent also goes through.
type gqlFakeEventsClient struct {
	listEvents func(context.Context, *gen.ListEventsRequest) (*gen.ListEventsResponse, error)
	getEvent   func(context.Context, *gen.GetEventRequest) (*gen.Event, error)
	gotList    *gen.ListEventsRequest
	gotGet     *gen.GetEventRequest
}

func (f *gqlFakeEventsClient) ListEvents(ctx context.Context, in *gen.ListEventsRequest, _ ...grpc.CallOption) (*gen.ListEventsResponse, error) {
	f.gotList = in
	if f.listEvents != nil {
		return f.listEvents(ctx, in)
	}
	return &gen.ListEventsResponse{}, nil
}

func (f *gqlFakeEventsClient) GetEvent(ctx context.Context, in *gen.GetEventRequest, _ ...grpc.CallOption) (*gen.Event, error) {
	f.gotGet = in
	if f.getEvent != nil {
		return f.getEvent(ctx, in)
	}
	return &gen.Event{}, nil
}

func (f *gqlFakeEventsClient) StreamEvents(context.Context, *gen.StreamEventsRequest, ...grpc.CallOption) (gen.Events_StreamEventsClient, error) {
	return nil, nil
}

// setEventsClientForTest swaps the package-level client and restores it, so
// these tests do not leak state into the rest of the package.
func setEventsClientForTest(t *testing.T, c gen.EventsClient) {
	t.Helper()
	prev := eventsClient
	eventsClient = c
	t.Cleanup(func() { eventsClient = prev })
}

// backendCode extracts the canonical code from a backend error.
func backendCode(t *testing.T, err error) httputil.ErrorCode {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	code, ok := ws.BackendErrorCode(err)
	if !ok {
		t.Fatalf("error %v carries no canonical code", err)
	}
	return code
}

// A syntactically valid Stellar contract strkey, so ValidateContractID passes
// and the test exercises the pass-through rather than the rejection path.
const testContractID = "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"

// ---------------------------------------------------------------------------
// ListEvents
// ---------------------------------------------------------------------------

func TestGraphQLBackend_ListEvents_PassesFiltersToGRPC(t *testing.T) {
	fake := &gqlFakeEventsClient{}
	setEventsClientForTest(t, fake)

	from, to := int64(100), int64(200)
	b := NewGraphQLBackend(nil)
	_, err := b.ListEvents(context.Background(), ws.EventsQuery{
		ContractID: testContractID,
		Topic0:     "t0",
		Topic1:     "t1",
		LedgerFrom: &from,
		LedgerTo:   &to,
		Limit:      25,
		Network:    "mainnet",
	})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}

	got := fake.gotList
	if got == nil {
		t.Fatal("gRPC client was never called")
	}
	if got.ContractId != testContractID {
		t.Errorf("contractId lost: %q", got.ContractId)
	}
	if got.Topic_0 != "t0" || got.Topic_1 != "t1" {
		t.Errorf("topic filters lost: %+v", got)
	}
	if got.Limit != 25 || got.Network != "mainnet" {
		t.Errorf("limit/network lost: limit=%d network=%q", got.Limit, got.Network)
	}
	if got.LedgerFrom != 100 || got.LedgerTo != 200 {
		t.Errorf("ledger range lost: from=%d to=%d", got.LedgerFrom, got.LedgerTo)
	}
}

// The cursor arrives opaque and must be decoded to the internal paging token
// before it reaches gRPC — the same encoding GET /v1/events uses, so a cursor
// from either transport is meaningful to the other.
func TestGraphQLBackend_ListEvents_DecodesOpaqueCursor(t *testing.T) {
	fake := &gqlFakeEventsClient{}
	setEventsClientForTest(t, fake)

	b := NewGraphQLBackend(nil)
	if _, err := b.ListEvents(context.Background(), ws.EventsQuery{
		Cursor: cursor.Encode("12345"),
		Limit:  10,
	}); err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if fake.gotList.Cursor != "12345" {
		t.Errorf("cursor = %q, want the decoded paging token 12345", fake.gotList.Cursor)
	}
}

func TestGraphQLBackend_ListEvents_RejectsMalformedCursor(t *testing.T) {
	setEventsClientForTest(t, &gqlFakeEventsClient{})
	b := NewGraphQLBackend(nil)
	_, err := b.ListEvents(context.Background(), ws.EventsQuery{Cursor: "!!!not-base64!!!", Limit: 10})
	if code := backendCode(t, err); code != httputil.INVALID_ARGUMENT {
		t.Errorf("code = %s, want INVALID_ARGUMENT", code)
	}
}

func TestGraphQLBackend_ListEvents_RejectsMalformedContractID(t *testing.T) {
	setEventsClientForTest(t, &gqlFakeEventsClient{})
	b := NewGraphQLBackend(nil)
	_, err := b.ListEvents(context.Background(), ws.EventsQuery{ContractID: "not-a-contract", Limit: 10})
	if code := backendCode(t, err); code != httputil.INVALID_ARGUMENT {
		t.Errorf("code = %s, want INVALID_ARGUMENT", code)
	}
}

// nextCursor must be re-encoded opaque on the way out, and nil rather than
// empty when there is no further page — matching the REST envelope so one
// auto-pager rule works across both transports.
func TestGraphQLBackend_ListEvents_EncodesNextCursor(t *testing.T) {
	setEventsClientForTest(t, &gqlFakeEventsClient{
		listEvents: func(context.Context, *gen.ListEventsRequest) (*gen.ListEventsResponse, error) {
			return &gen.ListEventsResponse{
				Events:     []*gen.Event{{Id: "e1", ContractId: "C1"}},
				NextCursor: "999",
				HasMore:    true,
			}, nil
		},
	})

	b := NewGraphQLBackend(nil)
	page, err := b.ListEvents(context.Background(), ws.EventsQuery{Limit: 10})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if !page.HasMore {
		t.Error("hasMore lost")
	}
	if page.NextCursor == nil {
		t.Fatal("nextCursor is nil despite a further page")
	}
	if *page.NextCursor != cursor.Encode("999") {
		t.Errorf("nextCursor = %q, want the opaque encoding of 999", *page.NextCursor)
	}
	if len(page.Events) != 1 || page.Events[0]["contract_id"] != "C1" {
		t.Errorf("events = %#v", page.Events)
	}
}

func TestGraphQLBackend_ListEvents_NoNextCursorOnLastPage(t *testing.T) {
	setEventsClientForTest(t, &gqlFakeEventsClient{
		listEvents: func(context.Context, *gen.ListEventsRequest) (*gen.ListEventsResponse, error) {
			return &gen.ListEventsResponse{HasMore: false}, nil
		},
	})

	b := NewGraphQLBackend(nil)
	page, err := b.ListEvents(context.Background(), ws.EventsQuery{Limit: 10})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if page.NextCursor != nil {
		t.Errorf("nextCursor = %q, want nil on the last page", *page.NextCursor)
	}
	if page.Events == nil {
		t.Error("events must be an empty slice, never nil — it serialises as [] not null")
	}
}

func TestGraphQLBackend_ListEvents_NilClientIsUnavailable(t *testing.T) {
	setEventsClientForTest(t, nil)
	b := NewGraphQLBackend(nil)
	_, err := b.ListEvents(context.Background(), ws.EventsQuery{Limit: 10})
	if code := backendCode(t, err); code != httputil.UNAVAILABLE {
		t.Errorf("code = %s, want UNAVAILABLE", code)
	}
}

func TestGraphQLBackend_ListEvents_MapsGRPCError(t *testing.T) {
	setEventsClientForTest(t, &gqlFakeEventsClient{
		listEvents: func(context.Context, *gen.ListEventsRequest) (*gen.ListEventsResponse, error) {
			return nil, status.Error(codes.InvalidArgument, "bad filter")
		},
	})

	b := NewGraphQLBackend(nil)
	_, err := b.ListEvents(context.Background(), ws.EventsQuery{Limit: 10})
	if code := backendCode(t, err); code != httputil.INVALID_ARGUMENT {
		t.Errorf("code = %s, want INVALID_ARGUMENT", code)
	}
}

// ---------------------------------------------------------------------------
// GetEvent
// ---------------------------------------------------------------------------

const testEventID = "11111111-1111-4111-8111-111111111111"

func TestGraphQLBackend_GetEvent_ReturnsEvent(t *testing.T) {
	fake := &gqlFakeEventsClient{
		getEvent: func(context.Context, *gen.GetEventRequest) (*gen.Event, error) {
			return &gen.Event{Id: testEventID, ContractId: "C1", LedgerSequence: 42}, nil
		},
	}
	setEventsClientForTest(t, fake)

	b := NewGraphQLBackend(nil)
	ev, err := b.GetEvent(context.Background(), testEventID, "mainnet")
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if ev["contract_id"] != "C1" || ev["ledger_sequence"] != uint64(42) {
		t.Errorf("event = %#v", ev)
	}
	if fake.gotGet.Id != testEventID || fake.gotGet.Network != "mainnet" {
		t.Errorf("request = %+v", fake.gotGet)
	}
}

func TestGraphQLBackend_GetEvent_RejectsMalformedID(t *testing.T) {
	setEventsClientForTest(t, &gqlFakeEventsClient{})
	b := NewGraphQLBackend(nil)
	_, err := b.GetEvent(context.Background(), "not-a-uuid", "mainnet")
	if code := backendCode(t, err); code != httputil.INVALID_ARGUMENT {
		t.Errorf("code = %s, want INVALID_ARGUMENT", code)
	}
}

// A real 404 resolves to a null field, the GraphQL convention for "no such
// object", rather than a transport error.
func TestGraphQLBackend_GetEvent_NotFoundIsNilNotError(t *testing.T) {
	setEventsClientForTest(t, &gqlFakeEventsClient{
		getEvent: func(context.Context, *gen.GetEventRequest) (*gen.Event, error) {
			return nil, status.Error(codes.NotFound, "no such event")
		},
	})

	b := NewGraphQLBackend(nil)
	ev, err := b.GetEvent(context.Background(), testEventID, "mainnet")
	if err != nil {
		t.Fatalf("a missing event must not be an error: %v", err)
	}
	if ev != nil {
		t.Errorf("event = %#v, want nil", ev)
	}
}

// Issue #227: a timeout or outage must not masquerade as a missing event —
// that turns a retryable backend failure into a permanent-looking null.
func TestGraphQLBackend_GetEvent_OutageIsNotNotFound(t *testing.T) {
	for _, tc := range []struct {
		name string
		code codes.Code
	}{
		{"deadline exceeded", codes.DeadlineExceeded},
		{"unavailable", codes.Unavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setEventsClientForTest(t, &gqlFakeEventsClient{
				getEvent: func(context.Context, *gen.GetEventRequest) (*gen.Event, error) {
					return nil, status.Error(tc.code, "backend down")
				},
			})

			b := NewGraphQLBackend(nil)
			ev, err := b.GetEvent(context.Background(), testEventID, "mainnet")
			if err == nil {
				t.Fatalf("a %s must surface as an error, not a null event (got %#v)", tc.name, ev)
			}
			if code := backendCode(t, err); code == httputil.NOT_FOUND {
				t.Error("a backend outage was reported as NOT_FOUND")
			}
		})
	}
}

func TestGraphQLBackend_GetEvent_NilClientIsUnavailable(t *testing.T) {
	setEventsClientForTest(t, nil)
	b := NewGraphQLBackend(nil)
	_, err := b.GetEvent(context.Background(), testEventID, "mainnet")
	if code := backendCode(t, err); code != httputil.UNAVAILABLE {
		t.Errorf("code = %s, want UNAVAILABLE", code)
	}
}

// ---------------------------------------------------------------------------
// ContractStats
// ---------------------------------------------------------------------------

// A nil DB degrades only contractStats; the event operations keep working,
// which is why the backend checks it per-operation rather than at construction.
func TestGraphQLBackend_ContractStats_NilDBIsUnavailable(t *testing.T) {
	b := NewGraphQLBackend(nil)
	_, err := b.ContractStats(context.Background(), ws.StatsQuery{Limit: 10})
	if code := backendCode(t, err); code != httputil.UNAVAILABLE {
		t.Errorf("code = %s, want UNAVAILABLE", code)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func TestEventToMap(t *testing.T) {
	if got := eventToMap(nil); got != nil {
		t.Errorf("eventToMap(nil) = %#v, want nil", got)
	}

	got := eventToMap(&EventJSON{
		ID:              "e1",
		ContractID:      "C1",
		LedgerSequence:  42,
		LedgerTimestamp: "2026-01-01T00:00:00Z",
		TransactionHash: "tx",
		EventIndex:      3,
		EventType:       "contract",
		Topics:          []string{"transfer"},
		Data:            "{}",
		CreatedAt:       "2026-01-02T00:00:00Z",
	})

	want := map[string]any{
		"id":               "e1",
		"contract_id":      "C1",
		"ledger_sequence":  uint64(42),
		"ledger_timestamp": "2026-01-01T00:00:00Z",
		"transaction_hash": "tx",
		"event_index":      uint32(3),
		"event_type":       "contract",
		"data":             "{}",
		"created_at":       "2026-01-02T00:00:00Z",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %#v, want %#v", k, got[k], v)
		}
	}
	topics, ok := got["topics"].([]string)
	if !ok || len(topics) != 1 || topics[0] != "transfer" {
		t.Errorf("topics = %#v", got["topics"])
	}
}

func TestGRPCToBackendError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want httputil.ErrorCode
	}{
		{"invalid argument", status.Error(codes.InvalidArgument, "bad"), httputil.INVALID_ARGUMENT},
		{"not found", status.Error(codes.NotFound, "gone"), httputil.NOT_FOUND},
		{"unavailable", status.Error(codes.Unavailable, "down"), httputil.UNAVAILABLE},
		{"unclassified", errors.New("boom"), httputil.INTERNAL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code := backendCode(t, grpcToBackendError(tc.err, "failed")); code != tc.want {
				t.Errorf("code = %s, want %s", code, tc.want)
			}
		})
	}
}

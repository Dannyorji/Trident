package handlers_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Depo-dev/trident/services/api/gen"
	"github.com/Depo-dev/trident/services/api/handlers"
	"github.com/Depo-dev/trident/services/api/internal/contracttest"
	"github.com/Depo-dev/trident/services/api/middleware"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ContractTestMockEventsClient is a simple mock for contract testing
type ContractTestMockEventsClient struct {
	ListEventsFunc func(context.Context, *gen.ListEventsRequest) (*gen.ListEventsResponse, error)
	GetEventFunc   func(context.Context, *gen.GetEventRequest) (*gen.Event, error)
}

func (m *ContractTestMockEventsClient) ListEvents(ctx context.Context, req *gen.ListEventsRequest, opts ...grpc.CallOption) (*gen.ListEventsResponse, error) {
	if m.ListEventsFunc != nil {
		return m.ListEventsFunc(ctx, req)
	}
	return &gen.ListEventsResponse{}, nil
}

func (m *ContractTestMockEventsClient) GetEvent(ctx context.Context, req *gen.GetEventRequest, opts ...grpc.CallOption) (*gen.Event, error) {
	if m.GetEventFunc != nil {
		return m.GetEventFunc(ctx, req)
	}
	return &gen.Event{}, nil
}

func (m *ContractTestMockEventsClient) StreamEvents(ctx context.Context, req *gen.StreamEventsRequest, opts ...grpc.CallOption) (gen.Events_StreamEventsClient, error) {
	return nil, nil
}

// withRouterMatchableHost sets the scheme/host fields contracttest's
// gorillamux-based router needs to resolve a request to a documented route
// (httptest.NewRequest alone leaves these unset), matching the pattern
// already established in contract_xcache_test.go.
func withRouterMatchableHost(req *http.Request) *http.Request {
	req.URL.Scheme = "http"
	req.URL.Host = "localhost:3000"
	req.Host = "localhost:3000"
	return req
}

// wrapRateLimited mirrors main.go's real middleware chain closely enough for
// contract testing: several documented 200 responses require the
// X-RateLimit-* headers TieredRateLimit adds, which the bare handler under
// test doesn't set on its own (issue #242). Local to this file since
// contract_xcache_test.go's identical helper lives in package handlers, not
// handlers_test.
func wrapRateLimited(h http.Handler) http.Handler {
	cfg := middleware.RateLimitConfig{
		SliderFn: func(_ context.Context, _ string, limit, _ int64) (bool, int64, error) {
			return true, 1, nil
		},
		Tiers: map[string]middleware.TierConfig{"free": {RPS: 1000, Window: time.Second}},
	}
	return middleware.TieredRateLimit(cfg)(h)
}

// TestContract_OpenAPIResponseValidation validates that real handler
// responses match the OpenAPI specification (issue #611). Previously this
// only checked that the body was valid JSON and that the path/method/status
// existed in the spec, never the actual response *shape* — which is exactly
// how three incompatible error envelopes (the canonical
// httputil.WriteErrorCtx one, a local {"error":{"message"}} helper with no
// code/request_id, and several raw http.Error/http.NotFound plain-text
// bodies) shipped against one documented API undetected. Now uses the
// shared contracttest package (already the house convention in
// contract_xcache_test.go, health_test.go, routes_inventory_test.go), which
// runs the real kin-openapi request/response validator against the live
// spec — full schema conformance, not just "valid JSON and the path
// exists".
func TestContract_OpenAPIResponseValidation(t *testing.T) {
	doc := contracttest.LoadSpec(t)
	router := contracttest.NewRouter(t, doc)

	// Test GET /v1/health response
	t.Run("GET /v1/health", func(t *testing.T) {
		req := withRouterMatchableHost(httptest.NewRequest(http.MethodGet, "/v1/health", nil))
		rr := httptest.NewRecorder()

		handlers.Health()(rr, req)

		contracttest.ValidateResponse(t, router, req, rr.Code, rr.Header(), rr.Body.Bytes())
	})

	// Test GET /v1/events response with mock gRPC client
	t.Run("GET /v1/events", func(t *testing.T) {
		mock := &ContractTestMockEventsClient{
			ListEventsFunc: func(ctx context.Context, req *gen.ListEventsRequest) (*gen.ListEventsResponse, error) {
				return &gen.ListEventsResponse{
					Events: []*gen.Event{
						{
							Id:              "550e8400-e29b-41d4-a716-446655440000",
							ContractId:      "CA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ",
							LedgerSequence:  1000,
							LedgerTimestamp: "2024-01-01T00:00:00Z",
							TransactionHash: "abcd1234",
							EventIndex:      0,
							EventType:       "contract",
							Topics:          []string{"transfer"},
							Data:            `{"amount":"100"}`,
							CreatedAt:       "2024-01-01T00:00:01Z",
						},
					},
					NextCursor: "",
					HasMore:    false,
				}, nil
			},
		}
		handlers.SetEventsClient(mock)

		req := withRouterMatchableHost(httptest.NewRequest(http.MethodGet, "/v1/events?limit=1", nil))
		req.Header.Set("X-API-Key", "contract-test-key")
		rr := httptest.NewRecorder()

		wrapRateLimited(http.HandlerFunc(handlers.ListEvents)).ServeHTTP(rr, req)

		contracttest.ValidateResponse(t, router, req, rr.Code, rr.Header(), rr.Body.Bytes())
	})

	// Test GET /v1/stats/contracts response
	t.Run("GET /v1/stats/contracts", func(t *testing.T) {
		// This endpoint requires DB and Redis, so we'll skip if not available
		req := withRouterMatchableHost(httptest.NewRequest(http.MethodGet, "/v1/stats/contracts?limit=1", nil))
		rr := httptest.NewRecorder()

		handlers.ContractsStats(nil, nil)(rr, req)

		if rr.Code == http.StatusOK {
			contracttest.ValidateResponse(t, router, req, rr.Code, rr.Header(), rr.Body.Bytes())
		}
	})
}

// TestContract_ErrorResponseValidation validates that error responses match
// the OpenAPI specification
func TestContract_ErrorResponseValidation(t *testing.T) {
	doc := contracttest.LoadSpec(t)
	router := contracttest.NewRouter(t, doc)

	t.Run("GET /v1/events/{id} - 404 error", func(t *testing.T) {
		mock := &ContractTestMockEventsClient{
			GetEventFunc: func(ctx context.Context, req *gen.GetEventRequest) (*gen.Event, error) {
				return nil, status.Error(codes.NotFound, "event not found")
			},
		}
		handlers.SetEventsClient(mock)

		mux := http.NewServeMux()
		mux.HandleFunc("GET /v1/events/{id}", handlers.GetEvent)

		req := withRouterMatchableHost(httptest.NewRequest(http.MethodGet, "/v1/events/550e8400-e29b-41d4-a716-446655440000", nil))
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)

		contracttest.ValidateResponse(t, router, req, rr.Code, rr.Header(), rr.Body.Bytes())
	})
}

// Route<->spec parity is enforced by TestEveryRouteIsDocumentedOrExempted and
// TestSpecHasNoPhantomOperations (services/api/routes_inventory_test.go),
// which derive the implemented-route set from the live registration table in
// routes.go rather than from a hand-maintained list. The previous
// TestContract_RouteParity kept exactly such a list here ("should be kept in
// sync with main.go") — the drift this suite exists to make structurally
// impossible — and is superseded by the table-driven tests (issue #513).

package main

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Depo-dev/trident/services/api/internal/contracttest"
)

// nonNilUnconnectedDB returns a *sql.DB that is non-nil but never actually
// dials: sql.Open validates the DSN string lazily and connects only when a
// query runs, so this is safe to use for handler paths that only need
// db != nil to proceed past their "database unavailable" check (createWebhook's
// invalid-body 400 is checked before any query is issued).
func nonNilUnconnectedDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", "postgres://user:pass@127.0.0.1:1/testdb")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// withRouterMatchableHost sets the scheme/host fields contracttest's
// gorillamux-based router needs to resolve a request to a documented route
// (httptest.NewRequest alone leaves these unset), matching the pattern
// established in handlers/contract_xcache_test.go.
func withRouterMatchableHost(req *http.Request) *http.Request {
	req.URL.Scheme = "http"
	req.URL.Host = "localhost:3000"
	req.Host = "localhost:3000"
	return req
}

// TestWebhooksContract_ErrorResponsesConformToSpec guards against issue
// #611: every webhooks.go handler used to answer errors with a raw
// http.Error/http.NotFound plain-text body, which api/openapi.yaml
// documented as "(plain-text body)" rather than fixing — the spec was
// written to match the bug. Both the handlers and the spec were updated to
// the canonical httputil.WriteErrorCtx envelope; this proves each documented
// error path on the webhook surface actually returns a body conforming to
// what the spec (now correctly) describes.
func TestWebhooksContract_ErrorResponsesConformToSpec(t *testing.T) {
	doc := contracttest.LoadSpec(t)
	router := contracttest.NewRouter(t, doc)

	t.Run("GET /v1/webhooks - 503 database unavailable", func(t *testing.T) {
		req := withRouterMatchableHost(httptest.NewRequest(http.MethodGet, "/v1/webhooks", nil))
		rr := httptest.NewRecorder()

		listWebhooksHandler(nil).ServeHTTP(rr, req)

		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503, got %d: %s", rr.Code, rr.Body.String())
		}
		contracttest.ValidateResponse(t, router, req, rr.Code, rr.Header(), rr.Body.Bytes())
	})

	t.Run("POST /v1/webhooks - 503 database unavailable", func(t *testing.T) {
		req := withRouterMatchableHost(httptest.NewRequest(http.MethodPost, "/v1/webhooks", nil))
		rr := httptest.NewRecorder()

		createWebhookHandler(nil).ServeHTTP(rr, req)

		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503, got %d: %s", rr.Code, rr.Body.String())
		}
		contracttest.ValidateResponse(t, router, req, rr.Code, rr.Header(), rr.Body.Bytes())
	})

	t.Run("POST /v1/webhooks/{id}/rotate-secret - 503 database unavailable", func(t *testing.T) {
		req := withRouterMatchableHost(httptest.NewRequest(http.MethodPost, "/v1/webhooks/550e8400-e29b-41d4-a716-446655440000/rotate-secret", nil))
		req.SetPathValue("id", "550e8400-e29b-41d4-a716-446655440000")
		rr := httptest.NewRecorder()

		rotateWebhookSecretHandler(nil).ServeHTTP(rr, req)

		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503, got %d: %s", rr.Code, rr.Body.String())
		}
		contracttest.ValidateResponse(t, router, req, rr.Code, rr.Header(), rr.Body.Bytes())
	})

	t.Run("GET /v1/webhooks/{id}/deliveries - 503 database unavailable", func(t *testing.T) {
		req := withRouterMatchableHost(httptest.NewRequest(http.MethodGet, "/v1/webhooks/550e8400-e29b-41d4-a716-446655440000/deliveries", nil))
		req.SetPathValue("id", "550e8400-e29b-41d4-a716-446655440000")
		rr := httptest.NewRecorder()

		deliveriesWebhookHandler(nil).ServeHTTP(rr, req)

		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503, got %d: %s", rr.Code, rr.Body.String())
		}
		contracttest.ValidateResponse(t, router, req, rr.Code, rr.Header(), rr.Body.Bytes())
	})

	t.Run("DELETE /v1/webhooks/{id} - 503 database unavailable", func(t *testing.T) {
		req := withRouterMatchableHost(httptest.NewRequest(http.MethodDelete, "/v1/webhooks/550e8400-e29b-41d4-a716-446655440000", nil))
		req.SetPathValue("id", "550e8400-e29b-41d4-a716-446655440000")
		rr := httptest.NewRecorder()

		deleteWebhookHandler(nil).ServeHTTP(rr, req)

		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503, got %d: %s", rr.Code, rr.Body.String())
		}
		contracttest.ValidateResponse(t, router, req, rr.Code, rr.Header(), rr.Body.Bytes())
	})

	t.Run("DELETE /v1/webhooks/{id} - 400 invalid id", func(t *testing.T) {
		req := withRouterMatchableHost(httptest.NewRequest(http.MethodDelete, "/v1/webhooks/not-a-uuid", nil))
		req.SetPathValue("id", "not-a-uuid")
		rr := httptest.NewRecorder()

		deleteWebhookHandler(nil).ServeHTTP(rr, req)

		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
		}
		contracttest.ValidateResponse(t, router, req, rr.Code, rr.Header(), rr.Body.Bytes())
	})

	t.Run("PATCH /v1/webhooks/{id}/pause - 503 database unavailable", func(t *testing.T) {
		req := withRouterMatchableHost(httptest.NewRequest(http.MethodPatch, "/v1/webhooks/550e8400-e29b-41d4-a716-446655440000/pause", nil))
		req.SetPathValue("id", "550e8400-e29b-41d4-a716-446655440000")
		rr := httptest.NewRecorder()

		pauseWebhookHandler(nil).ServeHTTP(rr, req)

		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503, got %d: %s", rr.Code, rr.Body.String())
		}
		contracttest.ValidateResponse(t, router, req, rr.Code, rr.Header(), rr.Body.Bytes())
	})

	t.Run("PATCH /v1/webhooks/{id}/resume - 503 database unavailable", func(t *testing.T) {
		req := withRouterMatchableHost(httptest.NewRequest(http.MethodPatch, "/v1/webhooks/550e8400-e29b-41d4-a716-446655440000/resume", nil))
		req.SetPathValue("id", "550e8400-e29b-41d4-a716-446655440000")
		rr := httptest.NewRecorder()

		resumeWebhookHandler(nil).ServeHTTP(rr, req)

		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503, got %d: %s", rr.Code, rr.Body.String())
		}
		contracttest.ValidateResponse(t, router, req, rr.Code, rr.Header(), rr.Body.Bytes())
	})

	t.Run("GET /v1/webhooks/{id}/dead-letters - 503 database unavailable", func(t *testing.T) {
		req := withRouterMatchableHost(httptest.NewRequest(http.MethodGet, "/v1/webhooks/550e8400-e29b-41d4-a716-446655440000/dead-letters", nil))
		req.SetPathValue("id", "550e8400-e29b-41d4-a716-446655440000")
		rr := httptest.NewRecorder()

		deadLettersWebhookHandler(nil).ServeHTTP(rr, req)

		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503, got %d: %s", rr.Code, rr.Body.String())
		}
		contracttest.ValidateResponse(t, router, req, rr.Code, rr.Header(), rr.Body.Bytes())
	})

	t.Run("POST /v1/webhooks/{id}/dead-letters/{deliveryId}/replay - 503 database unavailable", func(t *testing.T) {
		req := withRouterMatchableHost(httptest.NewRequest(http.MethodPost, "/v1/webhooks/550e8400-e29b-41d4-a716-446655440000/dead-letters/42/replay", nil))
		req.SetPathValue("id", "550e8400-e29b-41d4-a716-446655440000")
		req.SetPathValue("deliveryId", "42")
		rr := httptest.NewRecorder()

		replayDeadLetterHandler(nil).ServeHTTP(rr, req)

		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503, got %d: %s", rr.Code, rr.Body.String())
		}
		contracttest.ValidateResponse(t, router, req, rr.Code, rr.Header(), rr.Body.Bytes())
	})

	t.Run("POST /v1/webhooks - 400 invalid body", func(t *testing.T) {
		req := withRouterMatchableHost(httptest.NewRequest(http.MethodPost, "/v1/webhooks", nil))
		req.Body = http.NoBody
		rr := httptest.NewRecorder()

		createWebhookHandler(nonNilUnconnectedDB(t)).ServeHTTP(rr, req)

		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
		}
		contracttest.ValidateResponse(t, router, req, rr.Code, rr.Header(), rr.Body.Bytes())
	})
}

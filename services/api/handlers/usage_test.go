package handlers_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Depo-dev/trident/services/api/handlers"
	"github.com/Depo-dev/trident/services/api/middleware"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// unconnectedPool returns a *pgxpool.Pool that has never dialled a server.
// pgxpool.New only parses the DSN and defers connecting until first use, so
// this is safe to use in tests that only exercise guard clauses (missing
// admin key, bad input, ...) which return before any query is issued.
func unconnectedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://user:pass@127.0.0.1:1/db")
	if err != nil {
		t.Fatalf("construct unconnected pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestKeyUsage_NotConfigured_Returns503(t *testing.T) {
	h := handlers.KeyUsage(handlers.UsageConfig{})

	req := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	rr := httptest.NewRecorder()
	h(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("want 503, got %d", rr.Code)
	}
}

func TestKeyUsage_NoAuthenticatedKey_Returns501(t *testing.T) {
	h := handlers.KeyUsage(handlers.UsageConfig{DB: unconnectedPool(t)})

	req := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	rr := httptest.NewRecorder()
	h(rr, req)

	if rr.Code != http.StatusNotImplemented {
		t.Errorf("want 501 when no DB-backed key id is on the request context, got %d", rr.Code)
	}
}

// TestKeyUsage_LegacyEnvKey_Returns501NotServerError is the regression test
// for issue #616's usage.go half: a legacy env-var authenticated request
// now reaches KeyUsage with a non-empty, non-UUID sentinel id
// (middleware.LegacyEnvKeyID) instead of "". Before the dedicated check,
// that sentinel would fall through the idStr == "" guard and fail
// uuid.Parse, misreporting the caller's lack of per-key usage data as a 500
// "invalid authenticated key id" server error. It must instead get a clear
// 501 same family as the "no key at all" case, not a 500.
func TestKeyUsage_LegacyEnvKey_Returns501NotServerError(t *testing.T) {
	h := handlers.KeyUsage(handlers.UsageConfig{DB: unconnectedPool(t)})

	req := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	req = req.WithContext(middleware.WithAPIKeyID(req.Context(), middleware.LegacyEnvKeyID))
	rr := httptest.NewRecorder()
	h(rr, req)

	if rr.Code != http.StatusNotImplemented {
		t.Errorf("want 501 for a legacy env-var key, got %d", rr.Code)
	}
}

func TestKeyUsage_InvalidWindow_Returns400(t *testing.T) {
	h := handlers.KeyUsage(handlers.UsageConfig{DB: unconnectedPool(t)})

	req := httptest.NewRequest(http.MethodGet, "/v1/usage?from=not-a-date", nil)
	req = req.WithContext(middleware.WithAPIKeyID(req.Context(), uuid.NewString()))
	rr := httptest.NewRecorder()
	h(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", rr.Code)
	}
}

func TestAdminKeyUsageRollup_NotConfigured_Returns503(t *testing.T) {
	h := handlers.AdminKeyUsageRollup(handlers.AdminConfig{})

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/keys/"+uuid.NewString()+"/usage-rollup", nil)
	req.SetPathValue("id", uuid.NewString())
	rr := httptest.NewRecorder()
	h(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("want 503, got %d", rr.Code)
	}
}

func TestAdminKeyUsageRollup_MissingKey_Returns401(t *testing.T) {
	h := handlers.AdminKeyUsageRollup(handlers.AdminConfig{AdminKey: "secret", DB: unconnectedPool(t)})

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/keys/"+uuid.NewString()+"/usage-rollup", nil)
	req.SetPathValue("id", uuid.NewString())
	rr := httptest.NewRecorder()
	h(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", rr.Code)
	}
}

func TestAdminKeyUsageRollup_InvalidID_Returns400(t *testing.T) {
	h := handlers.AdminKeyUsageRollup(handlers.AdminConfig{AdminKey: "secret", DB: unconnectedPool(t)})

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/keys/not-a-uuid/usage-rollup", nil)
	req.SetPathValue("id", "not-a-uuid")
	req.Header.Set("X-Admin-Key", "secret")
	rr := httptest.NewRecorder()
	h(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", rr.Code)
	}
}

func TestAdminKeyUsageRollup_InvalidWindow_Returns400(t *testing.T) {
	h := handlers.AdminKeyUsageRollup(handlers.AdminConfig{AdminKey: "secret", DB: unconnectedPool(t)})

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/keys/"+uuid.NewString()+"/usage-rollup?to=garbage", nil)
	req.SetPathValue("id", uuid.NewString())
	req.Header.Set("X-Admin-Key", "secret")
	rr := httptest.NewRecorder()
	h(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", rr.Code)
	}
}

// TestKeyUsage_UnknownParam_Returns400 guards against issue #615: an
// unrecognised query parameter is a client bug (a typo'd ?form= silently
// ignored would hide itself), so it must be rejected, matching the sibling
// AdminKeyUsage handler's existing RejectUnknownParams convention rather than
// silently falling through to the default window.
func TestKeyUsage_UnknownParam_Returns400(t *testing.T) {
	h := handlers.KeyUsage(handlers.UsageConfig{DB: unconnectedPool(t)})

	req := httptest.NewRequest(http.MethodGet, "/v1/usage?form=2024-01-01T00:00:00Z", nil)
	req = req.WithContext(middleware.WithAPIKeyID(req.Context(), uuid.NewString()))
	rr := httptest.NewRecorder()
	h(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("want 400 for an unrecognised query parameter, got %d", rr.Code)
	}
}

func TestAdminKeyUsageRollup_UnknownParam_Returns400(t *testing.T) {
	h := handlers.AdminKeyUsageRollup(handlers.AdminConfig{AdminKey: "secret", DB: unconnectedPool(t)})

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/keys/"+uuid.NewString()+"/usage-rollup?form=2024-01-01T00:00:00Z", nil)
	req.SetPathValue("id", uuid.NewString())
	req.Header.Set("X-Admin-Key", "secret")
	rr := httptest.NewRecorder()
	h(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("want 400 for an unrecognised query parameter, got %d", rr.Code)
	}
}

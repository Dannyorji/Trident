package handlers_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Depo-dev/trident/services/api/handlers"
)

func TestInternalStatus_NotConfigured_Returns401(t *testing.T) {
	t.Setenv("INTERNAL_API_KEY", "")

	req := httptest.NewRequest(http.MethodGet, "/internal/status", nil)
	rr := httptest.NewRecorder()
	handlers.InternalStatus()(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("want 401 when INTERNAL_API_KEY unset, got %d", rr.Code)
	}
}

func TestInternalStatus_MissingKey_Returns401(t *testing.T) {
	t.Setenv("INTERNAL_API_KEY", "internal-secret")

	req := httptest.NewRequest(http.MethodGet, "/internal/status", nil)
	rr := httptest.NewRecorder()
	handlers.InternalStatus()(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("want 401 for missing X-Internal-Key, got %d", rr.Code)
	}
}

func TestInternalStatus_WrongKey_Returns401(t *testing.T) {
	t.Setenv("INTERNAL_API_KEY", "internal-secret")

	req := httptest.NewRequest(http.MethodGet, "/internal/status", nil)
	req.Header.Set("X-Internal-Key", "wrong-key")
	rr := httptest.NewRecorder()
	handlers.InternalStatus()(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("want 401 for wrong X-Internal-Key, got %d", rr.Code)
	}
}

func TestInternalStatus_ValidKey_Returns200(t *testing.T) {
	t.Setenv("INTERNAL_API_KEY", "internal-secret")

	req := httptest.NewRequest(http.MethodGet, "/internal/status", nil)
	req.Header.Set("X-Internal-Key", "internal-secret")
	rr := httptest.NewRecorder()
	handlers.InternalStatus()(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("want 200 for valid X-Internal-Key, got %d", rr.Code)
	}
}

// TestInternalStatus_NoRawKeyLeakage sends a distinctive, known
// X-Internal-Key value (both a wrong one on a 401 response and the correct
// one on a 200 response) and asserts the raw key string never appears
// anywhere in the response body.
func TestInternalStatus_NoRawKeyLeakage(t *testing.T) {
	const rawInternalKey = "trident-internal-status-super-secret-value"
	t.Setenv("INTERNAL_API_KEY", rawInternalKey)

	t.Run("wrong key on 401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/internal/status", nil)
		req.Header.Set("X-Internal-Key", "definitely-not-the-right-key")
		rr := httptest.NewRecorder()
		handlers.InternalStatus()(rr, req)

		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("want 401, got %d", rr.Code)
		}
		if strings.Contains(rr.Body.String(), rawInternalKey) {
			t.Errorf("raw internal key leaked into 401 response body: %q", rr.Body.String())
		}
	})

	t.Run("correct key on 200", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/internal/status", nil)
		req.Header.Set("X-Internal-Key", rawInternalKey)
		rr := httptest.NewRecorder()
		handlers.InternalStatus()(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("want 200, got %d", rr.Code)
		}
		if strings.Contains(rr.Body.String(), rawInternalKey) {
			t.Errorf("raw internal key leaked into 200 response body: %q", rr.Body.String())
		}
	})
}

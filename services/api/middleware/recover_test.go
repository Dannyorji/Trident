package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Depo-dev/trident/services/api/internal/httputil"
	"github.com/Depo-dev/trident/services/api/internal/metrics"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestRecover_NoPanic_PassesThrough(t *testing.T) {
	handler := Recover(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/fine", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Fatalf("expected body %q, got %q", "ok", rec.Body.String())
	}
}

func TestRecover_Panic_Returns500WithErrorEnvelope(t *testing.T) {
	before := testutil.ToFloat64(metrics.PanicsRecoveredTotal)

	handler := Recover(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/panics", nil)
	req = req.WithContext(httputil.ContextWithRequestID(req.Context(), "test-request-id-123"))

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}

	var body struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode body: %v", err)
	}
	if body.Error.Code != "INTERNAL" {
		t.Fatalf("expected error.code INTERNAL, got %q", body.Error.Code)
	}
	if body.Error.RequestID != "test-request-id-123" {
		t.Fatalf("expected error.request_id to be propagated, got %q", body.Error.RequestID)
	}

	after := testutil.ToFloat64(metrics.PanicsRecoveredTotal)
	if after != before+1 {
		t.Fatalf("expected PanicsRecoveredTotal to increment by 1, went from %v to %v", before, after)
	}
}

func TestRecover_Panic_ProcessSurvivesAndServesNextRequest(t *testing.T) {
	calls := 0
	handler := Recover(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			panic("first request panics")
		}
		w.WriteHeader(http.StatusOK)
	}))

	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, httptest.NewRequest(http.MethodGet, "/panics", nil))
	if rec1.Code != http.StatusInternalServerError {
		t.Fatalf("expected first request to get 500, got %d", rec1.Code)
	}

	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/fine", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected second request to succeed after recovery, got %d", rec2.Code)
	}
}

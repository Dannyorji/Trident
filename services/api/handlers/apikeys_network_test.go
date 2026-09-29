package handlers_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Depo-dev/trident/services/api/handlers"
)

// TestCreateAPIKey_RejectsUnknownNetwork guards against issue #252: before
// this, an unvalidated "network" field in the request body was written
// straight into api_keys.network, so a typo silently created a key scoped to
// an invisible network partition instead of failing the request.
func TestCreateAPIKey_RejectsUnknownNetwork(t *testing.T) {
	pool := connectRealTestDB(t)
	cfg := handlers.APIKeyConfig{AdminKey: testAdminKey, DB: pool}
	handler := handlers.CreateAPIKey(cfg)

	body := strings.NewReader(`{"label":"network-validation-test","network":"tesnet"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/api-keys", body)
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown network, got %d: %s", rec.Code, rec.Body.String())
	}

	var count int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM api_keys WHERE label = $1`, "network-validation-test",
	).Scan(&count); err != nil {
		t.Fatalf("query api_keys: %v", err)
	}
	if count != 0 {
		t.Fatalf("rejected request must not create a row, found %d", count)
	}
}

// TestCreateAPIKey_AcceptsKnownNetwork is the counterpart: a valid network
// still creates the key as before.
func TestCreateAPIKey_AcceptsKnownNetwork(t *testing.T) {
	pool := connectRealTestDB(t)
	cfg := handlers.APIKeyConfig{AdminKey: testAdminKey, DB: pool}
	handler := handlers.CreateAPIKey(cfg)

	body := strings.NewReader(`{"label":"network-validation-ok-test","network":"testnet"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/api-keys", body)
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(t.Context(), `DELETE FROM api_keys WHERE id = $1`, resp.ID)
	})

	var network string
	if err := pool.QueryRow(t.Context(),
		`SELECT network FROM api_keys WHERE id = $1`, resp.ID,
	).Scan(&network); err != nil {
		t.Fatalf("query created row: %v", err)
	}
	if network != "testnet" {
		t.Fatalf("expected network=testnet, got %q", network)
	}
}

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Depo-dev/trident/services/api/middleware"
)

// TestCreateWebhookHandler_RejectsUnknownNetwork guards against issue #252:
// before this, "network" flowed straight from the request body into
// webhook_subscriptions.network with no validation, so a typo could create a
// subscription scoped to an invisible network partition.
func TestCreateWebhookHandler_RejectsUnknownNetwork(t *testing.T) {
	db := connectWebhookTestDB(t)
	ctx := context.Background()

	var apiKeyID string
	if err := db.QueryRowContext(ctx,
		`INSERT INTO api_keys (key_hash, key_prefix, label) VALUES ($1, $2, $3) RETURNING id`,
		fmt.Sprintf("webhook-network-hash-%d", time.Now().UnixNano()),
		"test-prefix",
		"webhook-network-test",
	).Scan(&apiKeyID); err != nil {
		t.Fatalf("insert test api key: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM webhook_subscriptions WHERE api_key_id = $1`, apiKeyID)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM api_keys WHERE id = $1`, apiKeyID)
	})

	handler := createWebhookHandler(db)

	body := strings.NewReader(`{"contractId":"CCONTRACTX","targetUrl":"https://example.com/hook","network":"tesnet"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks", body)
	req = req.WithContext(middleware.WithAPIKeyID(req.Context(), apiKeyID))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown network, got %d: %s", rec.Code, rec.Body.String())
	}

	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM webhook_subscriptions WHERE api_key_id = $1`, apiKeyID,
	).Scan(&count); err != nil {
		t.Fatalf("query webhook_subscriptions: %v", err)
	}
	if count != 0 {
		t.Fatalf("rejected request must not create a row, found %d", count)
	}
}

// TestCreateWebhookHandler_AcceptsKnownNetwork is the counterpart: a valid
// network still creates the subscription as before.
func TestCreateWebhookHandler_AcceptsKnownNetwork(t *testing.T) {
	db := connectWebhookTestDB(t)
	ctx := context.Background()

	var apiKeyID string
	if err := db.QueryRowContext(ctx,
		`INSERT INTO api_keys (key_hash, key_prefix, label) VALUES ($1, $2, $3) RETURNING id`,
		fmt.Sprintf("webhook-network-ok-hash-%d", time.Now().UnixNano()),
		"test-prefix",
		"webhook-network-ok-test",
	).Scan(&apiKeyID); err != nil {
		t.Fatalf("insert test api key: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM webhook_subscriptions WHERE api_key_id = $1`, apiKeyID)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM api_keys WHERE id = $1`, apiKeyID)
	})

	handler := createWebhookHandler(db)

	body := strings.NewReader(`{"contractId":"CCONTRACTX","targetUrl":"https://example.com/hook","network":"mainnet"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks", body)
	req = req.WithContext(middleware.WithAPIKeyID(req.Context(), apiKeyID))
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

	var network string
	if err := db.QueryRowContext(ctx,
		`SELECT network FROM webhook_subscriptions WHERE id = $1`, resp.ID,
	).Scan(&network); err != nil {
		t.Fatalf("query created row: %v", err)
	}
	if network != "mainnet" {
		t.Fatalf("expected network=mainnet, got %q", network)
	}
}

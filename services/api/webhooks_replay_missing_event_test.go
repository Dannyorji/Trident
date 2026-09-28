package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Depo-dev/trident/services/api/middleware"
)

// TestReplayDeadLetterHandler_MissingEventFailsExplicitly proves issue #652:
// when the original event backing a dead-lettered delivery can no longer be
// loaded, the replay request fails explicitly instead of delivering a
// zero-valued stand-in payload to the subscriber's endpoint under a valid
// signature.
func TestReplayDeadLetterHandler_MissingEventFailsExplicitly(t *testing.T) {
	db := connectWebhookTestDB(t)
	ctx := context.Background()
	suffix := time.Now().UnixNano()

	var apiKeyID string
	if err := db.QueryRowContext(ctx,
		`INSERT INTO api_keys (key_hash, key_prefix, label) VALUES ($1, $2, $3) RETURNING id`,
		fmt.Sprintf("replay-missing-event-%d", suffix), "test-prefix", "replay-missing-event",
	).Scan(&apiKeyID); err != nil {
		t.Fatalf("insert api key: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_, _ = db.ExecContext(cleanupCtx, `DELETE FROM webhook_deliveries WHERE subscription_id IN (SELECT id FROM webhook_subscriptions WHERE api_key_id = $1)`, apiKeyID)
		_, _ = db.ExecContext(cleanupCtx, `DELETE FROM webhook_subscriptions WHERE api_key_id = $1`, apiKeyID)
		_, _ = db.ExecContext(cleanupCtx, `DELETE FROM api_keys WHERE id = $1`, apiKeyID)
	})

	var receiverHit bool
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receiverHit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()

	var subID string
	if err := db.QueryRowContext(ctx,
		`INSERT INTO webhook_subscriptions (api_key_id, contract_id, target_url, secret, network)
		 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		apiKeyID, "CREPLAYMISSINGEVENTTEST", receiver.URL, "secret", "testnet",
	).Scan(&subID); err != nil {
		t.Fatalf("insert subscription: %v", err)
	}

	// A dead-lettered delivery whose event_id references a soroban_events
	// row that does NOT exist (deleted/expired/never-inserted).
	nonExistentEventID := "00000000-0000-0000-0000-000000000000"
	var deliveryID int64
	if err := db.QueryRowContext(ctx, `
		INSERT INTO webhook_deliveries (subscription_id, event_id, attempt, attempts, status, success)
		VALUES ($1, $2, 5, 5, 'dead_lettered', false)
		RETURNING id
	`, subID, nonExistentEventID).Scan(&deliveryID); err != nil {
		t.Fatalf("insert dead-lettered delivery: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/"+subID+"/dead-letters/"+fmt.Sprint(deliveryID)+"/replay", nil)
	req = req.WithContext(middleware.WithAPIKeyID(req.Context(), apiKeyID))
	req.SetPathValue("id", subID)
	req.SetPathValue("deliveryId", fmt.Sprint(deliveryID))
	rr := httptest.NewRecorder()

	replayDeadLetterHandler(db).ServeHTTP(rr, req)

	if rr.Code == http.StatusOK {
		t.Fatalf("expected replay to fail when the original event cannot be loaded, got 200 OK: %s", rr.Body.String())
	}
	if receiverHit {
		t.Fatal("expected no delivery to the subscriber's endpoint when the original event is missing, but the receiver was hit")
	}
}

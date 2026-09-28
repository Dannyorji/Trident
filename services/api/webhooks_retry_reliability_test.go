package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestWebhookDeliveryIdempotency proves issue #649: a duplicate delivery
// record for the same (subscription_id, event_id, attempt) is rejected at
// the database level rather than silently accepted, and a crash between
// firing the HTTP call and recording it leaves a detectable "attempted"
// trace instead of no record at all.
func TestWebhookDeliveryIdempotency(t *testing.T) {
	db := connectWebhookTestDB(t)
	ctx := context.Background()

	subID, eventID := insertRetryTestFixtures(t, db)

	// Simulate "delivery attempted" being recorded before the HTTP call
	// fires, as runDeliveryAttempts now does.
	if err := recordWebhookAttemptStarted(ctx, db, subID, eventID, 1); err != nil {
		t.Fatalf("recordWebhookAttemptStarted: %v", err)
	}

	var status string
	if err := db.QueryRowContext(ctx,
		`SELECT status FROM webhook_deliveries WHERE subscription_id = $1 AND event_id = $2 AND attempt = 1`,
		subID, eventID,
	).Scan(&status); err != nil {
		t.Fatalf("query attempted row: %v", err)
	}
	if status != "pending" {
		t.Fatalf("expected pending status after attempt-started record, got %q", status)
	}

	// A raw duplicate INSERT (bypassing the ON CONFLICT-aware helpers) must
	// be rejected by the unique index, not silently accepted as a second row.
	_, err := db.ExecContext(ctx, `
		INSERT INTO webhook_deliveries (subscription_id, event_id, attempt, attempts, status, success)
		VALUES ($1, $2, 1, 1, 'pending', false)
	`, subID, eventID)
	if err == nil {
		t.Fatal("expected duplicate insert for same (subscription_id, event_id, attempt) to be rejected, got nil error")
	}

	// The real completion path updates the existing row in place.
	if err := recordWebhookDelivery(ctx, db, subID, eventID, 1, "success", webhookDeliveryResult{Success: true, StatusCode: 200}, nil); err != nil {
		t.Fatalf("recordWebhookDelivery: %v", err)
	}

	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM webhook_deliveries WHERE subscription_id = $1 AND event_id = $2 AND attempt = 1`,
		subID, eventID,
	).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 delivery row for the attempt, got %d", count)
	}
}

// TestResumePendingWebhookRetries proves issue #651: a delivery left
// mid-backoff (status 'failed', next_attempt_at in the past — simulating a
// process restart during the sleep) is picked up and resumed, not left
// stuck forever.
func TestResumePendingWebhookRetries(t *testing.T) {
	db := connectWebhookTestDB(t)
	ctx := context.Background()

	subID, eventID := insertRetryTestFixtures(t, db)

	previous := allowInsecureWebhookTargets
	allowInsecureWebhookTargets = true
	t.Cleanup(func() { allowInsecureWebhookTargets = previous })

	var delivered int
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		delivered++
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()

	if _, err := db.ExecContext(ctx, `UPDATE webhook_subscriptions SET target_url = $1 WHERE id = $2`, receiver.URL, subID); err != nil {
		t.Fatalf("update target_url: %v", err)
	}

	// Simulate attempt 1 having failed and its backoff having already
	// elapsed by the time the (simulated) restart happens.
	past := time.Now().Add(-time.Minute)
	if err := recordWebhookDelivery(ctx, db, subID, eventID, 1, "failed", webhookDeliveryResult{Err: fmt.Errorf("boom")}, &past); err != nil {
		t.Fatalf("seed failed attempt: %v", err)
	}

	resumePendingWebhookRetries(ctx, db)

	// resumePendingWebhookRetries dispatches resumption asynchronously;
	// poll briefly for the retry to land rather than sleeping a fixed guess.
	deadline := time.Now().Add(5 * time.Second)
	for delivered == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}

	if delivered == 0 {
		t.Fatal("expected resumePendingWebhookRetries to resume the mid-backoff delivery, but the receiver got no request")
	}

	var attempt2Status string
	if err := db.QueryRowContext(ctx,
		`SELECT status FROM webhook_deliveries WHERE subscription_id = $1 AND event_id = $2 AND attempt = 2`,
		subID, eventID,
	).Scan(&attempt2Status); err != nil {
		t.Fatalf("expected a recorded attempt 2 after resume: %v", err)
	}
	if attempt2Status != "success" {
		t.Fatalf("expected attempt 2 to succeed against the test receiver, got status %q", attempt2Status)
	}
}

func insertRetryTestFixtures(t *testing.T, db *sql.DB) (subscriptionID, eventID string) {
	t.Helper()
	ctx := context.Background()
	suffix := time.Now().UnixNano()

	var apiKeyID string
	if err := db.QueryRowContext(ctx,
		`INSERT INTO api_keys (key_hash, key_prefix, label) VALUES ($1, $2, $3) RETURNING id`,
		fmt.Sprintf("retry-reliability-%d", suffix), "test-prefix", "retry-reliability",
	).Scan(&apiKeyID); err != nil {
		t.Fatalf("insert api key: %v", err)
	}

	if err := db.QueryRowContext(ctx,
		`INSERT INTO webhook_subscriptions (api_key_id, contract_id, target_url, secret, network)
		 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		apiKeyID, "CRETRYRELIABILITYTEST", "https://example.invalid/hook", "secret", "testnet",
	).Scan(&subscriptionID); err != nil {
		t.Fatalf("insert subscription: %v", err)
	}

	if err := db.QueryRowContext(ctx,
		`INSERT INTO soroban_events (contract_id, ledger_sequence, ledger_timestamp, transaction_hash, event_index, event_type, network, topics, data)
		 VALUES ($1, $2, NOW(), $3, 0, 'contract', 'testnet', '[]'::jsonb, '{}'::jsonb) RETURNING id`,
		"CRETRYRELIABILITYTEST", suffix%1000000, fmt.Sprintf("tx-%d", suffix),
	).Scan(&eventID); err != nil {
		t.Fatalf("insert soroban event: %v", err)
	}

	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_, _ = db.ExecContext(cleanupCtx, `DELETE FROM webhook_deliveries WHERE subscription_id = $1`, subscriptionID)
		_, _ = db.ExecContext(cleanupCtx, `DELETE FROM webhook_subscriptions WHERE id = $1`, subscriptionID)
		_, _ = db.ExecContext(cleanupCtx, `DELETE FROM soroban_events WHERE id = $1`, eventID)
		_, _ = db.ExecContext(cleanupCtx, `DELETE FROM api_keys WHERE id = $1`, apiKeyID)
	})

	return subscriptionID, eventID
}

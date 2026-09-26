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

// TestWebhookHandlers_TenantScoping is the regression suite for issue #607:
// six webhook handlers (delete, pause, resume, deliveries, dead-letters,
// replay) previously queried and mutated webhook_subscriptions by id alone,
// with no api_key_id predicate, so any authenticated tenant could touch
// another tenant's webhook subscription simply by knowing (or guessing) its
// UUID. Each handler is proven here to return 404 (never 403 — a 403 would
// itself leak that the subscription exists) when tenant B's key is used
// against a subscription owned by tenant A, and to succeed when tenant A's
// own key is used against its own subscription.
func TestWebhookHandlers_TenantScoping(t *testing.T) {
	db := connectWebhookTestDB(t)
	ctx := context.Background()

	insertTestKey := func(label string) string {
		t.Helper()
		var id string
		if err := db.QueryRowContext(ctx,
			`INSERT INTO api_keys (key_hash, key_prefix, label) VALUES ($1, $2, $3) RETURNING id`,
			fmt.Sprintf("tenant-scoping-%s-%d", label, time.Now().UnixNano()),
			"test-prefix",
			"tenant-scoping-"+label,
		).Scan(&id); err != nil {
			t.Fatalf("insert test api key %s: %v", label, err)
		}
		t.Cleanup(func() {
			_, _ = db.ExecContext(context.Background(), `DELETE FROM webhook_deliveries WHERE subscription_id IN (SELECT id FROM webhook_subscriptions WHERE api_key_id = $1)`, id)
			_, _ = db.ExecContext(context.Background(), `DELETE FROM webhook_subscriptions WHERE api_key_id = $1`, id)
			_, _ = db.ExecContext(context.Background(), `DELETE FROM api_keys WHERE id = $1`, id)
		})
		return id
	}

	tenantA := insertTestKey("a")
	tenantB := insertTestKey("b")

	newSubscription := func(ownerID string) string {
		t.Helper()
		var id string
		if err := db.QueryRowContext(ctx,
			`INSERT INTO webhook_subscriptions (api_key_id, contract_id, target_url, secret, network)
			 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
			ownerID, "CTENANTSCOPINGTEST", "https://example.com/hook", "secret", "testnet",
		).Scan(&id); err != nil {
			t.Fatalf("insert test subscription: %v", err)
		}
		return id
	}

	newDeadLetter := func(subID string) string {
		t.Helper()
		var id string
		if err := db.QueryRowContext(ctx,
			`INSERT INTO webhook_deliveries (subscription_id, event_id, attempt, attempts, status, success)
			 VALUES ($1, $2, 1, 1, 'dead_lettered', false) RETURNING id`,
			subID, "00000000-0000-0000-0000-000000000000",
		).Scan(&id); err != nil {
			t.Fatalf("insert test dead-lettered delivery: %v", err)
		}
		return id
	}

	asTenant := func(req *http.Request, apiKeyID string) *http.Request {
		return req.WithContext(middleware.WithAPIKeyID(req.Context(), apiKeyID))
	}

	t.Run("DELETE scoped to owner: tenant B gets 404 on tenant A's subscription", func(t *testing.T) {
		subID := newSubscription(tenantA)
		req := httptest.NewRequest(http.MethodDelete, "/v1/webhooks/"+subID, nil)
		req.SetPathValue("id", subID)
		rec := httptest.NewRecorder()

		deleteWebhookHandler(db).ServeHTTP(rec, asTenant(req, tenantB))

		if rec.Code != http.StatusNotFound {
			t.Fatalf("tenant B delete: status = %d, want 404, body = %s", rec.Code, rec.Body.String())
		}

		// The subscription must still exist: a 404 alone doesn't prove the
		// delete was actually scoped rather than just, say, failing open.
		var stillExists bool
		if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM webhook_subscriptions WHERE id = $1)`, subID).Scan(&stillExists); err != nil {
			t.Fatalf("check subscription exists: %v", err)
		}
		if !stillExists {
			t.Fatal("tenant B's rejected delete request removed tenant A's subscription anyway")
		}
	})

	t.Run("DELETE scoped to owner: tenant A can delete its own subscription", func(t *testing.T) {
		subID := newSubscription(tenantA)
		req := httptest.NewRequest(http.MethodDelete, "/v1/webhooks/"+subID, nil)
		req.SetPathValue("id", subID)
		rec := httptest.NewRecorder()

		deleteWebhookHandler(db).ServeHTTP(rec, asTenant(req, tenantA))

		if rec.Code != http.StatusNoContent {
			t.Fatalf("tenant A delete: status = %d, want 204, body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("PAUSE scoped to owner: tenant B gets 404 on tenant A's subscription", func(t *testing.T) {
		subID := newSubscription(tenantA)
		req := httptest.NewRequest(http.MethodPatch, "/v1/webhooks/"+subID+"/pause", nil)
		req.SetPathValue("id", subID)
		rec := httptest.NewRecorder()

		pauseWebhookHandler(db).ServeHTTP(rec, asTenant(req, tenantB))

		if rec.Code != http.StatusNotFound {
			t.Fatalf("tenant B pause: status = %d, want 404, body = %s", rec.Code, rec.Body.String())
		}

		var pausedAtIsNull bool
		if err := db.QueryRowContext(ctx, `SELECT paused_at IS NULL FROM webhook_subscriptions WHERE id = $1`, subID).Scan(&pausedAtIsNull); err != nil {
			t.Fatalf("check paused_at: %v", err)
		}
		if !pausedAtIsNull {
			t.Fatal("tenant B's rejected pause request paused tenant A's subscription anyway")
		}
	})

	t.Run("RESUME scoped to owner: tenant B gets 404 on tenant A's subscription", func(t *testing.T) {
		subID := newSubscription(tenantA)
		if _, err := db.ExecContext(ctx, `UPDATE webhook_subscriptions SET paused_at = NOW() WHERE id = $1`, subID); err != nil {
			t.Fatalf("pre-pause test subscription: %v", err)
		}

		req := httptest.NewRequest(http.MethodPatch, "/v1/webhooks/"+subID+"/resume", nil)
		req.SetPathValue("id", subID)
		rec := httptest.NewRecorder()

		resumeWebhookHandler(db).ServeHTTP(rec, asTenant(req, tenantB))

		if rec.Code != http.StatusNotFound {
			t.Fatalf("tenant B resume: status = %d, want 404, body = %s", rec.Code, rec.Body.String())
		}

		var stillPaused bool
		if err := db.QueryRowContext(ctx, `SELECT paused_at IS NOT NULL FROM webhook_subscriptions WHERE id = $1`, subID).Scan(&stillPaused); err != nil {
			t.Fatalf("check paused_at: %v", err)
		}
		if !stillPaused {
			t.Fatal("tenant B's rejected resume request resumed tenant A's subscription anyway")
		}
	})

	t.Run("DELIVERIES scoped to owner: tenant B gets 404 on tenant A's subscription", func(t *testing.T) {
		subID := newSubscription(tenantA)
		req := httptest.NewRequest(http.MethodGet, "/v1/webhooks/"+subID+"/deliveries", nil)
		req.SetPathValue("id", subID)
		rec := httptest.NewRecorder()

		deliveriesWebhookHandler(db).ServeHTTP(rec, asTenant(req, tenantB))

		if rec.Code != http.StatusNotFound {
			t.Fatalf("tenant B deliveries: status = %d, want 404, body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("DEAD-LETTERS scoped to owner: tenant B gets 404 on tenant A's subscription", func(t *testing.T) {
		subID := newSubscription(tenantA)
		req := httptest.NewRequest(http.MethodGet, "/v1/webhooks/"+subID+"/dead-letters", nil)
		req.SetPathValue("id", subID)
		rec := httptest.NewRecorder()

		deadLettersWebhookHandler(db).ServeHTTP(rec, asTenant(req, tenantB))

		if rec.Code != http.StatusNotFound {
			t.Fatalf("tenant B dead-letters: status = %d, want 404, body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("REPLAY scoped to owner: tenant B gets 404 on tenant A's dead-lettered delivery", func(t *testing.T) {
		subID := newSubscription(tenantA)
		deliveryID := newDeadLetter(subID)

		req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/"+subID+"/dead-letters/"+deliveryID+"/replay", nil)
		req.SetPathValue("id", subID)
		req.SetPathValue("deliveryId", deliveryID)
		rec := httptest.NewRecorder()

		replayDeadLetterHandler(db).ServeHTTP(rec, asTenant(req, tenantB))

		if rec.Code != http.StatusNotFound {
			t.Fatalf("tenant B replay: status = %d, want 404, body = %s", rec.Code, rec.Body.String())
		}

		var stillDeadLettered bool
		if err := db.QueryRowContext(ctx, `SELECT status = 'dead_lettered' FROM webhook_deliveries WHERE id = $1`, deliveryID).Scan(&stillDeadLettered); err != nil {
			t.Fatalf("check delivery status: %v", err)
		}
		if !stillDeadLettered {
			t.Fatal("tenant B's rejected replay request replayed tenant A's delivery anyway")
		}
	})
}

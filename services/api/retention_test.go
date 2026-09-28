package main

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestLoadRetentionConfig_Defaults(t *testing.T) {
	for _, key := range []string{
		"RETENTION_AUDIT_LOG_DAYS",
		"RETENTION_PARSE_ERRORS_DAYS",
		"RETENTION_WEBHOOK_DELIVERIES_DAYS",
		"RETENTION_SOROBAN_EVENTS_DAYS",
		"RETENTION_EVENT_OUTBOX_DAYS",
	} {
		t.Setenv(key, "")
		_ = os.Unsetenv(key)
	}

	cfg := loadRetentionConfig()

	if cfg.AuditLogDays != 90 {
		t.Errorf("AuditLogDays = %d, want 90", cfg.AuditLogDays)
	}
	if cfg.ParseErrorsDays != 30 {
		t.Errorf("ParseErrorsDays = %d, want 30", cfg.ParseErrorsDays)
	}
	if cfg.WebhookDeliveriesDays != 30 {
		t.Errorf("WebhookDeliveriesDays = %d, want 30", cfg.WebhookDeliveriesDays)
	}
	// #645: previously defaulted to 0 (disabled), leaving the
	// highest-volume table to grow unbounded unless an operator opted in.
	if cfg.SorobanEventsDays != 90 {
		t.Errorf("SorobanEventsDays = %d, want 90", cfg.SorobanEventsDays)
	}
	// #604: event_outbox is the fastest-growing unbounded table, so unlike
	// soroban_events it defaults to enabled rather than opt-in.
	if cfg.EventOutboxDays != 7 {
		t.Errorf("EventOutboxDays = %d, want 7", cfg.EventOutboxDays)
	}
}

func TestLoadRetentionConfig_EventOutboxDaysOverride(t *testing.T) {
	t.Setenv("RETENTION_EVENT_OUTBOX_DAYS", "14")
	cfg := loadRetentionConfig()
	if cfg.EventOutboxDays != 14 {
		t.Errorf("EventOutboxDays = %d, want 14", cfg.EventOutboxDays)
	}
}

// TestEventOutboxRetention_OnlyDeletesPublishedRowsPastTheWindow is the
// regression test for #604's core deletion criterion: an unpublished row
// must never be deleted regardless of age (the relay has not yet delivered
// it), and a published row inside the retention window must survive.
func TestEventOutboxRetention_OnlyDeletesPublishedRowsPastTheWindow(t *testing.T) {
	db := connectWebhookTestDB(t) // reuses the shared TEST_DATABASE_URL helper
	ctx := context.Background()

	insert := func(published bool, publishedAt *time.Time) string {
		t.Helper()
		var id string
		if err := db.QueryRowContext(ctx,
			`INSERT INTO event_outbox (event_id, payload, published, published_at) VALUES (gen_random_uuid(), $1, $2, $3) RETURNING event_id`,
			`{"kind":"retention-test"}`, published, publishedAt,
		).Scan(&id); err != nil {
			t.Fatalf("insert event_outbox row: %v", err)
		}
		return id
	}

	oldPublishedAt := time.Now().Add(-10 * 24 * time.Hour) // 10 days old
	recentPublishedAt := time.Now().Add(-1 * time.Hour)    // 1 hour old

	oldPublished := insert(true, &oldPublishedAt)
	recentPublished := insert(true, &recentPublishedAt)
	oldUnpublished := insert(false, nil) // published_at is NULL when unpublished

	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM event_outbox WHERE event_id IN ($1, $2, $3)`, oldPublished, recentPublished, oldUnpublished)
	})

	// Mirror startRetentionJob's exact event_outbox query with a 7-day window.
	if _, err := db.ExecContext(ctx,
		`DELETE FROM event_outbox WHERE published = TRUE AND published_at < NOW() - ($1 || ' days')::INTERVAL AND event_id IN ($2, $3, $4)`,
		fmt.Sprintf("%d", 7), oldPublished, recentPublished, oldUnpublished,
	); err != nil {
		t.Fatalf("run retention delete: %v", err)
	}

	exists := func(id string) bool {
		var ok bool
		if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM event_outbox WHERE event_id = $1)`, id).Scan(&ok); err != nil {
			t.Fatalf("check exists: %v", err)
		}
		return ok
	}

	if exists(oldPublished) {
		t.Error("a published row older than the retention window was not deleted")
	}
	if !exists(recentPublished) {
		t.Error("a published row inside the retention window was deleted")
	}
	if !exists(oldUnpublished) {
		t.Error("an unpublished row was deleted despite never having been delivered")
	}
}

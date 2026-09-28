package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/Depo-dev/trident/services/api/middleware"
)

// connectWebhookTestDB mirrors handlers_test.connectRealTestDB (this is
// package main, which cannot import an internal test helper from another
// package): skip when TEST_DATABASE_URL is unset, hard-fail instead of
// silently skipping when REQUIRE_TEST_SERVICES is set.
func connectWebhookTestDB(t *testing.T) *sql.DB {
	t.Helper()
	url, ok := os.LookupEnv("TEST_DATABASE_URL")
	if !ok {
		if os.Getenv("REQUIRE_TEST_SERVICES") != "" {
			t.Fatal("TEST_DATABASE_URL must be set when REQUIRE_TEST_SERVICES is set")
		}
		t.Skip("SKIP: TEST_DATABASE_URL not set")
	}
	db, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("connect TEST_DATABASE_URL: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestListWebhooksHandler_Pagination(t *testing.T) {
	db := connectWebhookTestDB(t)
	ctx := context.Background()

	// resolveAPIKeyID reads api_key_id from context (populated by the real
	// NewDBAuth middleware, which calling a handler's ServeHTTP directly
	// bypasses) — see asTenant below. Create a real row so every request in
	// this test resolves to the same key.
	var apiKeyID string
	if err := db.QueryRowContext(ctx,
		`INSERT INTO api_keys (key_hash, key_prefix, label) VALUES ($1, $2, $3) RETURNING id`,
		fmt.Sprintf("webhook-pagination-hash-%d", time.Now().UnixNano()),
		"test-prefix",
		"webhook-pagination-test",
	).Scan(&apiKeyID); err != nil {
		t.Fatalf("insert test api key: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM webhook_subscriptions WHERE api_key_id = $1`, apiKeyID)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM api_keys WHERE id = $1`, apiKeyID)
	})

	// Five subscriptions, two sharing an identical created_at, proving the
	// id tiebreaker determines order at the tie (issue #220).
	tied := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	createdAts := []time.Time{
		tied.Add(4 * time.Second),
		tied,
		tied,
		tied.Add(-2 * time.Second),
		tied.Add(-6 * time.Second),
	}
	var ids []string
	for i, ts := range createdAts {
		var id string
		if err := db.QueryRowContext(ctx,
			`INSERT INTO webhook_subscriptions (api_key_id, contract_id, target_url, secret, network, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
			apiKeyID, fmt.Sprintf("CCONTRACT%d", i), "https://example.com/hook", "secret", "testnet", ts,
		).Scan(&id); err != nil {
			t.Fatalf("insert test webhook %d: %v", i, err)
		}
		ids = append(ids, id)
	}

	doPage := func(cursorParam string) listWebhooksResponse {
		t.Helper()
		url := "/v1/webhooks?limit=2"
		if cursorParam != "" {
			url += "&cursor=" + cursorParam
		}
		req := httptest.NewRequest(http.MethodGet, url, nil)
		req = req.WithContext(middleware.WithAPIKeyID(req.Context(), apiKeyID))
		rec := httptest.NewRecorder()
		listWebhooksHandler(db).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var resp listWebhooksResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode response: %v (body: %s)", err, rec.Body.String())
		}
		return resp
	}

	var seen []string
	cursor := ""
	for page := 0; ; page++ {
		if page > 10 {
			t.Fatal("pagination did not terminate")
		}
		resp := doPage(cursor)
		if len(resp.Webhooks) > 2 {
			t.Fatalf("page %d: returned %d webhooks, want at most limit=2", page, len(resp.Webhooks))
		}
		for _, wh := range resp.Webhooks {
			seen = append(seen, wh.ID)
		}
		if !resp.HasMore {
			if resp.NextCursor != nil {
				t.Fatal("has_more=false but next_cursor is set")
			}
			break
		}
		if resp.NextCursor == nil {
			t.Fatal("has_more=true but next_cursor is nil")
		}
		cursor = *resp.NextCursor
	}

	if len(seen) != len(ids) {
		t.Fatalf("saw %d webhooks across all pages, want %d: %v", len(seen), len(ids), seen)
	}

	wantOrder := []string{ids[0], maxIDStr(ids[1], ids[2]), minIDStr(ids[1], ids[2]), ids[3], ids[4]}
	for i := range wantOrder {
		if seen[i] != wantOrder[i] {
			t.Errorf("position %d: got %s, want %s (full order: %v)", i, seen[i], wantOrder[i], seen)
		}
	}
}

func TestListWebhooksHandler_ScopedToOwnAPIKey(t *testing.T) {
	db := connectWebhookTestDB(t)
	ctx := context.Background()

	var keyA, keyB string
	for i, dst := range []*string{&keyA, &keyB} {
		if err := db.QueryRowContext(ctx,
			`INSERT INTO api_keys (key_hash, key_prefix, label) VALUES ($1, $2, $3) RETURNING id`,
			fmt.Sprintf("webhook-scope-hash-%d-%d", time.Now().UnixNano(), i),
			"test-prefix",
			"webhook-scope-test",
		).Scan(dst); err != nil {
			t.Fatalf("insert test api key %d: %v", i, err)
		}
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM webhook_subscriptions WHERE api_key_id IN ($1, $2)`, keyA, keyB)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM api_keys WHERE id IN ($1, $2)`, keyA, keyB)
	})

	if _, err := db.ExecContext(ctx,
		`INSERT INTO webhook_subscriptions (api_key_id, contract_id, target_url, secret, network) VALUES ($1, $2, $3, $4, $5)`,
		keyA, "CCONTRACTA", "https://example.com/a", "secret", "testnet",
	); err != nil {
		t.Fatalf("insert webhook for key A: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/webhooks", nil)
	req = req.WithContext(middleware.WithAPIKeyID(req.Context(), keyB))
	rec := httptest.NewRecorder()
	listWebhooksHandler(db).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}

	var resp listWebhooksResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Webhooks) != 0 {
		t.Fatalf("key B should see 0 webhooks (key A's subscription must not leak across keys), got %d", len(resp.Webhooks))
	}
}

func maxIDStr(a, b string) string {
	if a > b {
		return a
	}
	return b
}

func minIDStr(a, b string) string {
	if a < b {
		return a
	}
	return b
}

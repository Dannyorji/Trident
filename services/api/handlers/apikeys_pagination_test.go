package handlers_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Depo-dev/trident/services/api/handlers"
)

const testAdminKey = "test-admin-key-for-pagination"

func TestListAPIKeys_Pagination(t *testing.T) {
	pool := connectRealTestDB(t)
	ctx := t.Context()

	label := fmt.Sprintf("pagination-test-%d", time.Now().UnixNano())

	// Five keys: two share an identical created_at, proving the id
	// tiebreaker (not just created_at) determines order and cursor position.
	tied := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	createdAts := []time.Time{
		tied.Add(4 * time.Second),
		tied,
		tied, // tie with the row above
		tied.Add(-2 * time.Second),
		tied.Add(-6 * time.Second),
	}

	var ids []string
	for i, ts := range createdAts {
		var id string
		err := pool.QueryRow(ctx,
			`INSERT INTO api_keys (key_hash, key_prefix, label, created_at)
			 VALUES ($1, $2, $3, $4) RETURNING id`,
			fmt.Sprintf("pagination-test-hash-%s-%d", label, i),
			"test-prefix",
			label,
			ts,
		).Scan(&id)
		if err != nil {
			t.Fatalf("insert test api key %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(t.Context(), `DELETE FROM api_keys WHERE label = $1`, label)
	})

	cfg := handlers.APIKeyConfig{AdminKey: testAdminKey, DB: pool}
	handler := handlers.ListAPIKeys(cfg)

	// Walk the full listing 2-at-a-time and collect every id seen, in order.
	var seen []string
	var seenLabelCount int
	cursor := ""
	for page := 0; ; page++ {
		if page > 10 {
			t.Fatal("pagination did not terminate — possible infinite loop")
		}
		url := "/v1/api-keys?limit=2"
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		req := httptest.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("X-Admin-Key", testAdminKey)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("page %d: status = %d, body = %s", page, rec.Code, rec.Body.String())
		}

		var resp handlers.ListAPIKeysResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("page %d: decode response: %v", page, err)
		}

		if len(resp.APIKeys) > 2 {
			t.Fatalf("page %d: returned %d keys, want at most limit=2", page, len(resp.APIKeys))
		}

		for _, k := range resp.APIKeys {
			if k.Label == label {
				seen = append(seen, k.ID)
				seenLabelCount++
			}
		}

		if !resp.HasMore {
			if resp.NextCursor != nil {
				t.Fatalf("page %d: has_more=false but next_cursor is set", page)
			}
			break
		}
		if resp.NextCursor == nil {
			t.Fatalf("page %d: has_more=true but next_cursor is nil", page)
		}
		cursor = *resp.NextCursor
	}

	if seenLabelCount != len(ids) {
		t.Fatalf("saw %d of this test's keys across all pages, want %d (dedup/skip bug)", seenLabelCount, len(ids))
	}

	// Order must be created_at DESC, with id DESC breaking the tie — i.e.
	// exactly the insertion order given createdAts is already sorted
	// descending except for the deliberate tie at index 1/2.
	wantOrder := []string{ids[0], maxID(ids[1], ids[2]), minID(ids[1], ids[2]), ids[3], ids[4]}
	if len(seen) != len(wantOrder) {
		t.Fatalf("collected %d ids, want %d: %v", len(seen), len(wantOrder), seen)
	}
	for i := range wantOrder {
		if seen[i] != wantOrder[i] {
			t.Errorf("position %d: got id %s, want %s (full order: %v)", i, seen[i], wantOrder[i], seen)
		}
	}
}

func TestListAPIKeys_EmptyDatabaseHasNoNextCursor(t *testing.T) {
	pool := connectRealTestDB(t)
	label := fmt.Sprintf("pagination-empty-test-%d", time.Now().UnixNano())

	cfg := handlers.APIKeyConfig{AdminKey: testAdminKey, DB: pool}
	handler := handlers.ListAPIKeys(cfg)

	req := httptest.NewRequest(http.MethodGet, "/v1/api-keys?limit=1000", nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var resp handlers.ListAPIKeysResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	// A huge limit against however many real keys exist must never report
	// more to come.
	if resp.HasMore || resp.NextCursor != nil {
		t.Fatalf("has_more=%v next_cursor=%v, want false/nil when the page covers everything", resp.HasMore, resp.NextCursor)
	}
	_ = label
}

func TestListAPIKeys_InvalidCursorIsRejected(t *testing.T) {
	pool := connectRealTestDB(t)
	cfg := handlers.APIKeyConfig{AdminKey: testAdminKey, DB: pool}
	handler := handlers.ListAPIKeys(cfg)

	req := httptest.NewRequest(http.MethodGet, "/v1/api-keys?cursor=not-a-valid-cursor", nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a malformed cursor", rec.Code)
	}
}

func maxID(a, b string) string {
	if a > b {
		return a
	}
	return b
}

func minID(a, b string) string {
	if a < b {
		return a
	}
	return b
}

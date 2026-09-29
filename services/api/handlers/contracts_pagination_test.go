package handlers_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Depo-dev/trident/services/api/handlers"
)

const testContractsAdminKey = "test-admin-key-for-contracts-pagination"

// TestListContracts_CursorIsOpaque asserts the acceptance criterion that was
// missing entirely before (issue #220): next_cursor must not be the raw
// database id in plain text.
func TestListContracts_CursorIsOpaque(t *testing.T) {
	pool := connectRealTestDB(t)
	ctx := t.Context()

	network := fmt.Sprintf("pagination-test-%d", time.Now().UnixNano())
	var ids []string
	for i := 0; i < 3; i++ {
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO indexed_contracts (contract_id, network) VALUES ($1, $2) RETURNING id`,
			fmt.Sprintf("CCONTRACT%d", i), network,
		).Scan(&id); err != nil {
			t.Fatalf("insert test contract %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(t.Context(), `DELETE FROM indexed_contracts WHERE network = $1`, network)
	})

	cfg := handlers.ContractConfig{AdminKey: testContractsAdminKey, DB: pool}
	handler := handlers.ListContracts(cfg)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/contracts?limit=2", nil)
	req.Header.Set("X-Admin-Key", testContractsAdminKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var resp handlers.ListContractsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.HasMore || resp.NextCursor == nil {
		t.Fatalf("expected has_more=true with a next_cursor, got has_more=%v next_cursor=%v", resp.HasMore, resp.NextCursor)
	}

	// The cursor must not be (or contain) any raw contract row id in plain
	// text — it must be opaque.
	for _, id := range ids {
		if strings.Contains(*resp.NextCursor, id) {
			t.Fatalf("next_cursor exposes a raw id in plain text: %q contains %q", *resp.NextCursor, id)
		}
	}
}

// TestListContracts_PaginatesAllRowsExactlyOnce is the full walk-the-listing
// regression: every inserted row must be seen exactly once across pages,
// using only the opaque cursor.
func TestListContracts_PaginatesAllRowsExactlyOnce(t *testing.T) {
	pool := connectRealTestDB(t)
	ctx := t.Context()

	network := fmt.Sprintf("pagination-walk-test-%d", time.Now().UnixNano())
	var ids []string
	for i := 0; i < 5; i++ {
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO indexed_contracts (contract_id, network) VALUES ($1, $2) RETURNING id`,
			fmt.Sprintf("CWALK%d", i), network,
		).Scan(&id); err != nil {
			t.Fatalf("insert test contract %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(t.Context(), `DELETE FROM indexed_contracts WHERE network = $1`, network)
	})

	cfg := handlers.ContractConfig{AdminKey: testContractsAdminKey, DB: pool}
	handler := handlers.ListContracts(cfg)

	seen := map[string]bool{}
	cursorParam := ""
	for page := 0; ; page++ {
		if page > 20 {
			t.Fatal("pagination did not terminate")
		}
		url := "/v1/admin/contracts?limit=2"
		if cursorParam != "" {
			url += "&cursor=" + cursorParam
		}
		req := httptest.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("X-Admin-Key", testContractsAdminKey)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d: status = %d, body = %s", page, rec.Code, rec.Body.String())
		}

		var resp handlers.ListContractsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("page %d: decode: %v", page, err)
		}
		for _, c := range resp.Contracts {
			if c.Network != nil && *c.Network == network {
				if seen[c.ID] {
					t.Fatalf("page %d: id %s seen twice", page, c.ID)
				}
				seen[c.ID] = true
			}
		}
		if !resp.HasMore {
			break
		}
		cursorParam = *resp.NextCursor
	}

	for _, id := range ids {
		if !seen[id] {
			t.Errorf("id %s was never returned across any page", id)
		}
	}
}

func TestListContracts_MalformedCursorIsRejected(t *testing.T) {
	pool := connectRealTestDB(t)
	cfg := handlers.ContractConfig{AdminKey: testContractsAdminKey, DB: pool}
	handler := handlers.ListContracts(cfg)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/contracts?cursor=!!!not-valid!!!", nil)
	req.Header.Set("X-Admin-Key", testContractsAdminKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a malformed cursor", rec.Code)
	}
}

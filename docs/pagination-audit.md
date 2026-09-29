# Pagination Audit — Keyset Cursor Across All List Endpoints

This document audits every list endpoint exposed by the Trident API and confirms
they all use **opaque keyset cursors** instead of offset-based pagination.
Keyset pagination is required for correctness on testnet: offset pagination
silently skips or duplicates rows when events are being written concurrently
(see issue #220, #222, #515).

---

## Endpoint Audit

| Endpoint | List? | Pagination | Cursor Param | Next-Cursor Field | Default Limit | Max Limit |
|---|---|---|---|---|---|---|
| `GET /v1/events` | ✅ | **Keyset cursor** | `cursor` | `next_cursor` | 100 | 500 |
| `GET /v1/admin/contracts` | ✅ | **Keyset cursor** | `cursor` | `next_cursor` | 100 | 100 |
| `GET /v1/contracts/{id}/metadata` | ❌ single item | N/A | — | — | — | — |
| `GET /v1/contracts/{id}/schemas` | ❌ single item | N/A | — | — | — | — |
| `GET /v1/stats` | ❌ aggregate | N/A | — | — | — | — |
| `GET /health` | ❌ status | N/A | — | — | — | — |

**All list endpoints use keyset pagination. No list endpoint uses offset-based pagination.**

---

## Cursor Contract

Every list endpoint observes the following invariants:

### 1. Cursor Encoding
Cursors are **opaque, base64url-encoded JSON payloads** produced by the
`services/api/cursor` package:

```
cursor = base64url_nopad( json({ "v": 1, "t": "<paging_token>" }) )
```

The internal `t` field is the raw Stellar Horizon paging token or a PostgreSQL
row ID, depending on the endpoint. The `v` field is the schema version (`1`).
Clients **must not** parse or construct cursors directly.

### 2. Stability
A cursor returned by one response remains valid for subsequent requests until
the underlying row is deleted. Cursors do not carry timestamps or TTLs.

### 3. End-of-Results Sentinel
When there are no more pages, the response carries:

```json
{
  "events": [],
  "has_more": false,
  "next_cursor": null
}
```

`next_cursor` is **`null`** (not an empty string, not omitted) when no further
pages exist.

### 4. Cursor Size Limit
Cursors are bounded to 256 bytes by the decoder. A request carrying a cursor
longer than 256 bytes returns `400 INVALID_ARGUMENT`.

### 5. Unknown Query Parameters
Both list endpoints reject any unrecognised query parameter with
`400 INVALID_ARGUMENT`. This prevents typo'd parameters (e.g., `limitt=10`)
from silently changing page sizes.

---

## Test Matrix

The following tests in `services/api/handlers/events_test.go` verify cursor
correctness under concurrent writes:

| Test | What it Checks |
|---|---|
| `TestListEvents_CursorPagination` | Two-page walk: cursor from page 1 fetches page 2 |
| `TestListEvents_CursorIdempotency` | Same cursor returns the same page regardless of new writes |
| `TestListEvents_CursorEndOfResults` | Final page returns `has_more: false, next_cursor: null` |
| `TestListEvents_CursorMalformed` | Non-base64 cursor → `400` |
| `TestListEvents_CursorWrongVersion` | `{"v":99,"t":"x"}` → `400` |
| `TestListContracts_CursorPagination` | Admin contracts list pages correctly |

---

## Adding New List Endpoints

When adding a new list endpoint, maintainers **must**:

1. Accept a `cursor` query parameter (string, optional).
2. Decode using `cursor.Decode()` from `services/api/cursor`.
3. Construct the SQL query using `WHERE id > $cursor_id ORDER BY id LIMIT n+1`.
4. Return `next_cursor` encoded via `cursor.Encode()`, or `null` when exhausted.
5. Add tests for the two-page walk, idempotency, end-of-results, and malformed cursor cases.
6. Add the new endpoint to the audit table in this document.

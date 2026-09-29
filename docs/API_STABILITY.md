# 📜 Trident `/v1/` API Compatibility Promise & Stability Policy

This document formally defines the **frozen `/v1/` API surface, backward-compatibility guarantees, deprecation lifecycle, and versioning contract** for Trident across the REST API, WebSocket streams, and official client SDKs.

---

## 1. Frozen `/v1/` Public API Surface

Trident commits that all endpoints listed below are **stable and frozen**. Any valid request targeting these endpoints will remain supported without breaking changes throughout the lifetime of the `v1` API.

### 1.1 Stable Endpoints

| Category | Method & Path | Stability Guarantee | Description |
|---|---|---|---|
| **System & Health** | `GET /v1/health` | **Frozen** | Healthcheck reporting database, redis, and indexer lag |
| | `GET /v1/status` | **Frozen** | Current network, sync ledger height, and version |
| **Event Ingestion** | `GET /v1/events` | **Frozen** | Filtered historical event queries with keyset pagination |
| | `GET /v1/events/{id}` | **Frozen** | Fetch single normalized Soroban event by UUID |
| | `POST /v1/events/batch` | **Frozen** | Bulk event query by contract lists and topics |
| | `GET /v1/events/stream` | **Frozen** | Real-time SSE / WebSocket streaming event delivery |
| **Contracts** | `GET /v1/contracts/{id}/spec` | **Frozen** | Decoded contract specification and XDR interface |
| | `GET /v1/contracts/{id}/events/schema` | **Frozen** | Extracted topic and value event schemas |
| | `GET /v1/contracts/{id}/storage` | **Frozen** | Snapshot of current contract instance/persistent storage |
| **Statistics** | `GET /v1/stats/indexer` | **Frozen** | Ingestion throughput and ledger indexing stats |
| | `GET /v1/stats/contracts` | **Frozen** | Contract invocation and event volume aggregates |
| **Auth & Keys** | `POST /v1/api-keys` | **Frozen** | Cryptographic API key generation |
| | `GET /v1/api-keys` | **Frozen** | List active API keys with usage metrics |
| | `POST /v1/api-keys/{id}/rotate` | **Frozen** | Zero-downtime key rotation with overlap window |
| | `PATCH /v1/api-keys/{id}` | **Frozen** | Update key label or rate-limit tier |
| | `DELETE /v1/api-keys/{id}` | **Frozen** | Immediate key revocation and cache eviction |

---

## 2. Invariants & Compatibility Guarantees

### 2.1 Request & Parameter Contracts
- **No Required Field Additions**: Trident will never add new required query parameters, path variables, or request body fields to existing `v1` endpoints.
- **Strict Parameter Validation**: Unknown query parameters are rejected with `400 INVALID_ARGUMENT` rather than silently ignored.
- **Data Formatting**:
  - Contract addresses: Stellar contract strkey (`C` followed by 55 base32 uppercase characters).
  - Identifiers: Canonical RFC 4122 UUID v4.
  - Timestamps: ISO-8601 / RFC 3339 UTC strings (`YYYY-MM-DDTHH:MM:SSZ`).
  - Ledgers: 32-bit unsigned integer sequences.

### 2.2 Keyset Pagination Semantics
- All paginated endpoints return `events` (or item list), `next_cursor`, and `has_more: boolean`.
- Cursors are opaque base64 tokens encoding `(ledger_sequence, event_id)` ensuring stable deterministic sorting and zero duplicate/skipped records during active ingestion.
- `limit` parameter supports values between `1` and `200` (default `50`).

### 2.3 Error Envelope & Codes
All errors conform to the standardized error envelope:
```json
{
  "error": {
    "code": "INVALID_ARGUMENT",
    "message": "limit must be an integer between 1 and 200"
  }
}
```

The error codes are frozen:
- `INVALID_ARGUMENT` (`400`): Malformed input, illegal type, or out-of-bound limit.
- `UNAUTHORIZED` (`401`): Missing, invalid, or revoked API key.
- `FORBIDDEN` (`403`): Key tier lacks permission or quota exhausted.
- `NOT_FOUND` (`404`): Resource or event ID does not exist.
- `RATE_LIMIT_EXCEEDED` (`429`): Request exceeded per-minute or daily quota.
- `UNAVAILABLE` (`503`): Storage, Redis, or RPC upstream temporarily unreachable.
- `INTERNAL` (`500`): Unhandled system error.

---

## 3. Explicitly Non-Stable & Experimental Surface

The following surfaces carry **no backward-compatibility promise** and may change or be refactored:

1. **Operational / Diagnostic Admin Endpoints**:
   - `GET /v1/admin/db` (Internal connection pooler diagnostics).
2. **Internal Wire Formats**:
   - Internal gRPC daemon protocols between the Rust Indexer and Go API.
3. **Draft Endpoints**:
   - Any endpoint annotated in OpenAPI with `x-experimental: true`.

---

## 4. Deprecation Policy & Sunset Notice

If a stable `v1` endpoint or response field must be retired:

1. **Notice Window**: Minimum **180 days** advance notice before removal.
2. **Deprecation Headers**: Every HTTP response will include standard RFC 8594 deprecation headers:
   ```http
   Deprecation: @1772323200
   Sunset: Wed, 01 Mar 2027 00:00:00 GMT
   Link: <https://docs.trident.telocel.com/migrations/v1-to-v2>; rel="deprecation"
   ```
3. **Documentation**: Detailed migration guides and alternative endpoints published in `CHANGELOG.md` and docs portal.

---

## 5. Breaking Change Versioning (`/v2/` Rules)

When a breaking architectural change is unavoidable:
- It will be introduced under a new major path: `/v2/`.
- `/v1/` and `/v2/` will run concurrently with full feature parity throughout the deprecation window.
- Existing `/v1/` clients and SDKs will continue to operate without code modifications.

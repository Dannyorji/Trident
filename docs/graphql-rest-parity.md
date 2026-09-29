# GraphQL & REST Event Query Parity Specification

This document establishes the **parity contract** between Trident's REST API (`GET /v1/events`, `GET /v1/stream`) and GraphQL interface (`graphql-transport-ws`). Both interfaces expose the same underlying Stellar Soroban event data stream and must produce identical data structures, observe the same authentication tiers, and enforce identical rate limits (issues #223, #427, #514).

---

## 1. Interface Mapping & Parity Matrix

| Feature | REST API (`/v1/events`, `/v1/stream`) | GraphQL (`graphql-transport-ws`) | Parity Status |
|---|---|---|---|
| **Event ID** | `id` (string, `<ledger>-<index>`) | `id` (string, `<ledger>-<index>`) | ✅ Identical format |
| **Contract ID** | `contract_id` (strkey `C...`) | `contractId` (strkey `C...`) | ✅ Identical value |
| **Ledger Sequence** | `ledger_sequence` (uint64) | `ledgerSequence` (Int / String) | ✅ Identical value |
| **Ledger Timestamp** | `ledger_timestamp` (RFC 3339) | `ledgerTimestamp` (RFC 3339) | ✅ Identical value |
| **Tx Hash** | `transaction_hash` (hex) | `transactionHash` (hex) | ✅ Identical value |
| **Topics** | `topics` (JSON string array) | `topics` (String array) | ✅ Identical value |
| **Data Payload** | `data` (ScVal decoded string/JSON) | `data` (String) | ✅ Identical value |
| **Created At** | `created_at` (RFC 3339) | `createdAt` (RFC 3339) | ✅ Identical value |

---

## 2. Authentication & Rate Limiting Parity

### Authentication
* **REST**: Provided via `X-API-Key` or `Authorization: Bearer <key>` header.
* **GraphQL**: Provided via `connection_init` payload: `{"type": "connection_init", "payload": {"authToken": "<key>"}}` or standard HTTP auth header on upgrade.
* **Tiers**: Both paths enforce the same tiered quotas:
  * **Free**: 60 req/min (or 5 concurrent subs)
  * **Pro**: 600 req/min (or 20 concurrent subs)
  * **Enterprise**: 3000 req/min (or 50 concurrent subs)

### Query Limits & Guards
* **REST**: Rejects queries exceeding `limit=500` or carrying unknown params with `400 INVALID_ARGUMENT`.
* **GraphQL**: Enforces `gqlMaxQueryLen = 8192` bytes and `gqlMaxSubsPerConn = 50` subscriptions per hijacked WebSocket connection.

---

## 3. Streaming & Event Delivery Parity

When an event matching contract `C...` is emitted:
* **SSE Stream (`/v1/stream?contractId=C...`)** sends:
  ```
  event: message
  data: {"id":"12345-0","contract_id":"C...","ledger_sequence":12345,"topics":["mint"],"data":"1000"}
  ```
* **GraphQL WS (`subscription { contractEvents(contractId: "C...") }`)** sends:
  ```json
  {
    "id": "1",
    "type": "next",
    "payload": {
      "data": {
        "contractEvents": {
          "id": "12345-0",
          "contractId": "C...",
          "ledgerSequence": 12345,
          "topics": ["mint"],
          "data": "1000"
        }
      }
    }
  }
  ```

---

## 4. Automated Parity Verification in CI

The test suite in `services/api/ws/graphql_test.go` runs on every pull request to guarantee:
1. **Event Data Encoding**: Event fields match character-for-character between REST JSON envelopes and GraphQL subscription payloads.
2. **Filter Matching**: Subscriptions with `topic0` filters only receive matching events identically to REST topic query parameters.
3. **Drop & Backpressure Behavior**: Slow consumers are dropped and cleaned up under equivalent buffer saturation policies.

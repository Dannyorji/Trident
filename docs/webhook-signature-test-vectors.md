# Webhook Signature Test Vectors

Published test vectors for the Trident webhook signing scheme (issue #452).
Use these to validate your receiver implementation offline before wiring it
up to a live subscription.

## Signing scheme

Every delivery POSTs a JSON body and two headers:

| Header | Description |
|---|---|
| `X-Trident-Timestamp` | Unix seconds (integer, decimal string) when the delivery was issued |
| `X-Trident-Signature` | `sha256=<lowercase hex>` — HMAC-SHA256 of the signed message (see below) |

**Signed message:** the timestamp and raw body bytes, concatenated:

```
message = "<timestamp>.<raw_body>"
```

where `<timestamp>` is the decimal integer from `X-Trident-Timestamp`, `.` is
a literal ASCII period, and `<raw_body>` is the exact bytes of the HTTP
request body — not re-serialised, not pretty-printed.

**HMAC key:** your subscription secret (`whsec_<hex>`, returned at subscription
creation and never re-exposed by the API).

**Reference implementation (Python):**

```python
import hmac, hashlib

def compute_signature(timestamp: int, body: bytes, secret: str) -> str:
    message = f"{timestamp}.".encode() + body
    mac = hmac.new(secret.encode(), message, hashlib.sha256)
    return "sha256=" + mac.hexdigest()
```

## Replay protection

The `X-Trident-Timestamp` header is bound into the signature, so an old
delivery cannot be replayed with a different timestamp. Receivers **must**
reject deliveries where the timestamp differs from the current wall clock by
more than the tolerance window. The recommended window is **300 seconds**
(5 minutes); all SDK helpers expose this as `DEFAULT_TOLERANCE_SECONDS`.

## Secret rotation

During an overlap window after `POST /v1/webhooks/{id}/rotate-secret`,
the `X-Trident-Signature` header contains **two** space-separated signatures:

```
X-Trident-Signature: sha256=<new_sig> sha256=<old_sig>
```

Receivers **must** accept a delivery if either token matches their active
secret. This allows you to swap your verification key before the old secret
expires. The SDK helpers handle this automatically — pass your current active
secret and they check all tokens.

## Verification procedure

1. **Read the raw body bytes** before any JSON parsing.
2. **Parse `X-Trident-Timestamp`** as a decimal integer. Reject if missing
   or non-numeric.
3. **Check the timestamp age.** `abs(now_unix - timestamp) > 300` → reject
   as a potential replay.
4. **Compute the expected signature:**
   `sha256=HMAC-SHA256(key=secret, message=f"{timestamp}.{raw_body}")`
5. **Compare** each space-separated token in `X-Trident-Signature` against
   the expected value using a **constant-time** comparison. Accept if any
   token matches.
6. **Return 2xx** on success; return 4xx (not 5xx) on rejection so Trident
   does not retry a rejected delivery.

## Test vectors

All vectors use the same signed-message format:
`message = f"{timestamp}.{body}"`

### Vector 1 — minimal payload

| Field | Value |
|---|---|
| **Secret** | `whsec_0000000000000000000000000000000000000000000000000000000000000001` |
| **Timestamp** | `1700000000` |
| **Body** | `{"id":"evt_test_001"}` |
| **Signed message** | `1700000000.{"id":"evt_test_001"}` |
| **Expected signature** | `sha256=0c5e53bcf4dd4338bc9b91cba384aed76c88fcb31a21ca28cf68ef19bef8797a` |

### Vector 2 — empty body

Confirms the scheme is well-defined when there are no events to deliver (e.g.
a ping delivery).

| Field | Value |
|---|---|
| **Secret** | `whsec_0000000000000000000000000000000000000000000000000000000000000001` |
| **Timestamp** | `1700000000` |
| **Body** | _(empty — zero bytes)_ |
| **Signed message** | `1700000000.` |
| **Expected signature** | `sha256=e7f81d9f74fad194220a4403cd3d75a2ee450ee8422648b4a0230a4ce77c2f5d` |

### Vector 3 — realistic payload

A full delivery payload as the server actually sends it.

| Field | Value |
|---|---|
| **Secret** | `whsec_deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef` |
| **Timestamp** | `1700000000` |
| **Body** | (see below) |
| **Expected signature** | `sha256=db74732f4339f7362266716ac2613c575d4a6640e710a1505f8afa3d3db78276` |

Body (minified, no trailing newline):

```json
{"id":"wh_1700000000000000000","webhook_id":"sub-abc123","event":{"id":"evt-xyz","contractId":"CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAD2KM","ledgerSequence":55001,"topic0":"transfer","data":{"amount":"100"},"txHash":"abc123def456","network":"testnet"},"timestamp":1700000000,"delivered_at":"2023-11-14T22:13:20Z"}
```

## SDK helpers

Each SDK ships a `webhook` module with `compute_signature` and
`verify_signature` (or equivalent) that implement this scheme and enforce the
tolerance window. All four include the test vectors above as test cases.

| SDK | Module | Function |
|---|---|---|
| Go | `sdk/go/webhook.go` | `VerifyWebhookSignature` |
| Python | `sdk/python/src/trident_indexer/webhook.py` | `verify_signature` |
| TypeScript | `sdk/typescript/src/webhook.ts` | `verifySignature` |
| Rust | `sdk/rust/src/webhook.rs` | `verify_signature` |

## Deriving your own vectors

```python
import hmac, hashlib

def compute_signature(timestamp: int, body: bytes, secret: str) -> str:
    msg = f"{timestamp}.".encode() + (body if isinstance(body, bytes) else body.encode())
    return "sha256=" + hmac.new(secret.encode(), msg, hashlib.sha256).hexdigest()

# Example
print(compute_signature(1700000000, b'{"id":"evt_test_001"}',
    "whsec_0000000000000000000000000000000000000000000000000000000000000001"))
# → sha256=0c5e53bcf4dd4338bc9b91cba384aed76c88fcb31a21ca28cf68ef19bef8797a
```

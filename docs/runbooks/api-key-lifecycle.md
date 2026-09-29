# API key lifecycle runbook

This runbook covers DB-backed consumer API keys created through
`POST /v1/api-keys`. It is the operator procedure for issuance, planned
rotation, revocation, and compromise response. `ADMIN_API_KEY` and legacy
`API_KEY_HASHES` credentials are separate secret classes; see
[Environment variables](../ENVIRONMENT.md) and
[API key hashing guarantees](../security-triage.md#api-key-hashing-salt-and-comparison-guarantees).

## Key contract

| Property | Contract |
| --- | --- |
| Plaintext format | `trident_` followed by 64 lowercase hexadecimal characters |
| Length | 72 characters |
| Entropy | 32 bytes (256 bits) read from Go's `crypto/rand` CSPRNG |
| Stored credential | Lowercase hexadecimal SHA-256 digest of the complete plaintext key |
| Display prefix | First 16 plaintext characters: `trident_` plus the first 8 random hex characters |
| Plaintext availability | Returned once in the create response; never stored in the database or returned by list operations |
| Active state | `api_keys.revoked_at IS NULL` |

The `trident_` marker makes the credential recognizable to secret scanners.
The non-secret `key_prefix` is the greppable identifier to use in inventories,
structured logs, and operational records without logging the full credential.
Operators may search the admin key list or database by prefix, resolve it to
the key UUID, and revoke that UUID. Prefixes are identifiers, not
authenticators: handle the unlikely case
of a collision by checking the label, network, creator, and creation time.
Never put the full key in logs, tickets, chat, command history, or labels.

## Why SHA-256 is deliberate

Plain SHA-256 is appropriate here because the input is a uniformly random
256-bit machine-generated secret. An attacker who obtains `key_hash` cannot
run a practical dictionary attack; the search space is the key's full 256
bits. SHA-256 also permits an indexed equality lookup on every request.

This reasoning does **not** apply to user passwords or other human-chosen,
low-entropy secrets. Those require a unique salt and a deliberately slow,
memory-hard password KDF such as Argon2id. Replacing SHA-256 with a slow KDF
for these random API keys would add request latency and denial-of-service cost
without fixing a realistic offline-guessing risk. If key generation ever
accepts user-selected material or reduces entropy, this decision must be
revisited before release.

## Issue a key

Use a descriptive label that identifies the consumer and purpose. Keep the
network and rate-limit tier identical to the key being replaced when rotating.

```bash
curl --fail-with-body -X POST "$TRIDENT_URL/v1/api-keys" \
  -H "X-Admin-Key: $ADMIN_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "label": "billing-worker production",
    "network": "mainnet",
    "rate_limit_tier": "standard",
    "created_by": "on-call@example.com"
  }'
```

Capture the returned `key` directly into the consumer's managed secret store.
Record only its `id`, `key_prefix`, label, owner, issue time, and planned
rotation date in the credential inventory. If the one-time plaintext response
is lost, revoke that key and create another; it cannot be recovered.

Some deployments also require an existing consumer `X-API-Key` on admin
routes because authentication middleware protects all `/v1/*` paths. In that
configuration, add the operator/bootstrap key as `-H "X-API-Key: ..."`; do not
reuse the newly issued consumer key as the admin secret.

## Planned rotation with an overlap window

Rotation is create-first and revoke-last. Creating or rotating a key does not alter the
old row, so both credentials remain valid during the overlap window.

### Option A: Dedicated Rotate Endpoint (`POST /v1/api-keys/{id}/rotate`)

Trident provides a native atomic rotation endpoint that clones the old key's network,
rate-limit tier, and metadata, creating a new plaintext credential in one operation:

```bash
curl --fail-with-body -X POST \
  "$TRIDENT_URL/v1/api-keys/$OLD_KEY_ID/rotate" \
  -H "X-Admin-Key: $ADMIN_API_KEY"
```

The response returns the new plaintext key and prefix while the old key remains fully active for zero-downtime cutover.

### Option B: Manual Issuance Workflow

1. List keys and record the old key's UUID and prefix. Confirm its consumer,
   network, and tier.
2. Create a new key using the issuance procedure above. Give it a label that
   distinguishes the rotation generation or issue date.
3. Store and deploy the new plaintext credential to every consumer. Do not
   revoke the old key yet. Set an explicit overlap deadline appropriate for
   the deployment rollout and rollback window.
4. Cut clients over to the new key and verify successful requests are
   attributed to the new UUID in `audit_log`.
5. During the overlap, check that new-key traffic is healthy and old-key
   traffic has stopped. Roll back to the still-valid old key if necessary.
6. After the overlap deadline and at least one normal traffic interval with no
   old-key use, revoke the old UUID:

   ```bash
   curl --fail-with-body -X DELETE \
     "$TRIDENT_URL/v1/api-keys/$OLD_KEY_ID" \
     -H "X-Admin-Key: $ADMIN_API_KEY"
   ```

7. Confirm the old key receives `401`, the new key still succeeds, and the
   admin key list shows `revoked_at` for the old UUID. Remove the old value
   from consumer secret stores and record the completed cutover.

Revocation is a soft delete: the service sets `revoked_at` and immediately
evicts the key's Redis authentication cache entry. The next authentication
attempt is rejected without waiting for the cache TTL.

## Suspected or confirmed compromise

Do not use an overlap window for a compromised credential. Preserve the
incident start time, then contain first:

1. Identify the key UUID from the reported prefix and inventory. If necessary,
   query `api_keys` by `key_prefix`; never ask for the full key in a ticket.
2. Immediately revoke the affected UUID with `DELETE /v1/api-keys/{id}`.
3. Verify requests using the compromised key now receive `401`.
4. Create a replacement, deploy it to legitimate consumers, and verify their
   recovery. Remove the compromised value from every managed secret store.
5. Audit usage from the earliest possible exposure time through revocation.

The admin usage endpoint provides an aggregate by endpoint:

```bash
curl --fail-with-body \
  "$TRIDENT_URL/v1/admin/keys/$KEY_ID/usage?from=$FROM_RFC3339&to=$TO_RFC3339" \
  -H "X-Admin-Key: $ADMIN_API_KEY"
```

For incident investigation, preserve and query the detailed audit rows:

```sql
SELECT ts, endpoint, method, ip, user_agent, status_code,
       duration_ms, result_count, request_id, network
FROM audit_log
WHERE api_key_id = '<affected-key-uuid>'
  AND ts >= '<earliest-exposure-rfc3339>'
  AND ts <  '<revocation-rfc3339>'
ORDER BY ts ASC;
```

Review unexpected IPs, user agents, endpoints, networks, result volume, and
request IDs; correlate request IDs with retained application logs. Preserve
the results according to incident-response policy, determine what data or
operations were accessible, and expand credential rotation if the key shared
a secret store or delivery channel with other credentials. Close the incident
only after the revoked key remains rejected and replacement-key activity is
accounted for.

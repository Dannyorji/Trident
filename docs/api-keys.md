# API Key Management & Security Model

This document describes the API key lifecycle in Trident: key generation, configuration, runtime validation, and zero-downtime rotation.

## Overview & Security Model

Trident validates incoming client API keys via the `X-API-Key` HTTP request header on protected endpoints.

### Key Security Principles

1. **Cryptographic Generation**: API keys are generated as 32 cryptographically random bytes, hex-encoded into 64-character strings.
2. **No Raw Key Storage**: The Go REST API process **never** stores or logs raw API keys in memory after startup. Only HMAC-SHA256 digests (calculated with `API_KEY_SALT`) or SHA-256 hashes (for database-managed keys) are retained or evaluated.
3. **Timing-Attack Resistance**: Verification uses `crypto/subtle.ConstantTimeCompare` across candidate valid key hashes to ensure constant-time comparison regardless of key correctness or position.

---

## Key Generation

Use `cmd/keygen` or the helper script `scripts/generate-api-key.sh` to generate a new valid API key pair:

```bash
# Using the helper script:
API_KEY_SALT="your-deployment-salt" ./scripts/generate-api-key.sh

# Or using go run directly:
go run ./services/api/cmd/keygen -salt "your-deployment-salt"
```

### Example Output

```
=== Trident API Key Generator ===
Raw API Key (client X-API-Key): 8f3a9b... (64 hex characters)
HMAC-SHA256 Hash (server config): c4e17... (64 hex characters)
Salt used:                       your-deployment-salt

Configuration instructions:
  API_KEY_SALT=your-deployment-salt
  API_KEY_HASHES=c4e17...
```

- **Client**: Pass the **Raw API Key** in the `X-API-Key` HTTP header.
- **Server**: Configure **`API_KEY_SALT`** and **`API_KEY_HASHES`** (or `API_KEY`) in your environment configuration.

---

## Environment Configuration

In your deployment environment (`.env`, Helm values, or Fly secrets):

```env
# Deployment secret for salting API key hashes
API_KEY_SALT=your-deployment-salt

# Comma-separated list of accepted HMAC-SHA256 key digests
API_KEY_HASHES=hash_1,hash_2

# Single key digest alternative (Phase 1 convenience)
API_KEY=hash_1
```

---

## Zero-Downtime Key Rotation

To rotate an existing API key or revoke a compromised key without downtime:

1. **Generate New Key Pair**: Run `cmd/keygen` using the current `API_KEY_SALT` to obtain a new raw key and its HMAC hash (`hash_new`).
2. **Update Server Environment**: Set `API_KEY_HASHES` to include both the active old hash and the new hash:
   ```env
   API_KEY_HASHES=hash_old,hash_new
   ```
   Deploy the server. The Go API now accepts requests signed by either key.
3. **Update Clients**: Transition client applications to send the new raw key in `X-API-Key`.
4. **Decommission Old Key**: Remove `hash_old` from `API_KEY_HASHES`:
   ```env
   API_KEY_HASHES=hash_new
   ```
   Redeploy the server to finalize rotation.

---

## Phase 1 vs Phase 2 Architecture

- **Phase 1 (Environment Variables)**: Static single or multi-key authentication via `API_KEY` / `API_KEY_HASHES` environment variables. Ideal for standalone or single-tenant deployments.
- **Phase 2 (Database-backed `api_keys`)**: Multi-tenant API key management via Postgres (`api_keys` table), rate-limiting tiers (Free, Pro, Internal), dynamic creation (`POST /v1/api-keys`), revocation (`DELETE /v1/api-keys/{id}`), and Redis caching.

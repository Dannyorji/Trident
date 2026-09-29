# Vulnerability Triage Process

How SBOM/vulnerability findings from `.github/workflows/security-scan.yml`
(issue #311) get triaged, allowlisted, or fixed.

## What runs, and when

| Check | Tool | Scope | Runs on |
|---|---|---|---|
| SBOM | [syft](https://github.com/anchore/syft) via `anchore/sbom-action` | Each of the 3 built images (go-api, grpc-api, indexer) | push/PR to `dev`/`main`, weekly |
| Image scan | [trivy](https://github.com/aquasecurity/trivy) | Same 3 images | push/PR to `dev`/`main`, weekly |
| Image secret audit | `docker history` plus image config inspection | Same 4 images | push/PR to `dev`/`main`, weekly |
| Rust deps | `cargo audit` (`rustsec/audit-check`) | Workspace `Cargo.lock` | push/PR to `dev`/`main`, weekly |
| Go deps | `govulncheck` | `services/api`, `sdk/go` | push/PR to `dev`/`main`, weekly |
| npm deps | `npm audit` | `explorer`, `sdk/typescript`, `sdk/react` | push/PR to `dev`/`main`, weekly |
| Python deps | `pip-audit` | `sdk/python` | push/PR to `dev`/`main`, weekly |

The weekly schedule exists so a CVE disclosed *after* a PR merged still gets
caught — not just at the moment code changes.

## Severity gate

The image scan only fails the build on **fixable** `HIGH` or `CRITICAL`
findings (`--ignore-unfixed`). An unfixable base-image CVE with no upstream
patch available would otherwise block CI indefinitely with no action anyone
could take — those are tracked (see below) rather than gated on.

Dependency audits (`cargo audit`, `govulncheck`, `npm audit --audit-level=high`,
`pip-audit`) fail on any advisory at or above the tool's own "high" severity
classification — dependencies are far easier to bump than a base image, so
there's less reason to tolerate an unfixed one.

## When a scan fails

1. **Check if a fix is available.**
   - Image: bump the base image tag/digest in the relevant `Dockerfile` (`crates/api/Dockerfile`,
     `crates/indexer/Dockerfile`, `services/api/Dockerfile`).
   - Rust: `cargo update -p <crate>` (or the minimal version bump `cargo audit fix` suggests).
   - Go: `go get <module>@<patched-version>` in the affected `go.mod`.
   - npm: `npm audit fix` (or a manual version bump if `fix` would introduce a breaking change).
   - Python: bump the pinned version in `sdk/python/pyproject.toml`.

   Fix and re-run the scan. This is always preferred over allowlisting.

2. **If no fix is available yet** (upstream hasn't shipped one), allowlist it:
   - **Image findings** → add the CVE ID to `.trivyignore` at the repo root,
     with a comment above it recording:
     - the CVE ID
     - why it's not being fixed right now (no upstream patch / not reachable
       in how we use the package / confirmed false positive)
     - the date added
     - a re-review date (suggest +90 days, sooner for anything network-reachable)
   - **Dependency findings** — each tool has its own suppression mechanism:
     - `cargo audit`: add an `[advisories.ignore]` entry to a `.cargo/audit.toml`
       (create it if it doesn't exist yet) with the same comment convention as above.
     - `govulncheck`: no first-class ignore file — if genuinely unfixable, note
       it in this doc's table below and add a `//nolint`-style comment at the
       call site referencing the CVE, or exclude the specific module version
       via `go.mod` `exclude` only if truly necessary.
     - `npm audit`: add an `overrides` entry in the affected `package.json` if
       a transitive dependency can be forced to a patched version; otherwise
       track here.
     - `pip-audit`: `pip-audit --ignore-vuln <ID>` — wire the same flag into
       the workflow step's `args` if this comes up, with a comment here
       explaining why.

3. **Open a tracking issue** for anything allowlisted, so it doesn't silently
   live in `.trivyignore` (or equivalent) forever. Link the issue in the
   allowlist comment.

## Current allowlist

_(Kept empty until something is actually allowlisted — see `.trivyignore`
for the live list. Do not pre-populate this with hypothetical entries.)_

## Re-review cadence

Everything in `.trivyignore` (or another ecosystem's suppression file) should
carry a re-review date. Check `.trivyignore` and re-run
`docs/security-triage.md`'s table above quarterly at minimum, or immediately
when notified of a new advisory for an allowlisted package.

## API key hashing, salt, and comparison guarantees (issue #315)

This section documents how `services/api` authenticates callers, so future
changes to the auth path (see also issues #314, #316) start from the same
mental model. Operators should use the
[API key lifecycle runbook](runbooks/api-key-lifecycle.md) for issuance,
overlap rotation, revocation, and compromise response.

### Two key formats, two hashing schemes

- **DB-backed keys** (`api_keys` table, created via `POST /v1/api-keys`):
  the plaintext key (`trident_` + 32 random bytes, generated with
  `crypto/rand`) is hashed with **plain SHA-256, no salt**
  (`handlers.sha256hex`, `middleware.sha256KeyHash`) and only the hash is
  stored/looked up (`WHERE key_hash = $1`).

  No salt is used here, and that is intentional rather than an oversight:
  salting exists to defeat precomputed dictionaries/rainbow tables against
  *low-entropy secrets* (e.g. human-chosen passwords) and to stop the same
  password reused across sites from sharing a hash. A Trident API key is a
  256-bit `crypto/rand` value with no dictionary to precompute against, is
  never reused across services, and is never intended to be memorized —
  those are exactly the properties that make salting unnecessary for a
  password but essential; a high-entropy random key does not need it.

- **Legacy env-var keys** (`API_KEY_HASHES`): keys are hashed with
  **HMAC-SHA256 keyed by `API_KEY_SALT`** (`middleware.hmacKeyHash`). This
  path predates the DB-backed table and keeps `API_KEY_SALT` as the HMAC key
  so operators can rotate every legacy key's effective hash by rotating one
  environment variable, without needing per-key salts.

### Why hash-map lookup is not a raw-key timing oracle

Both auth paths compare a *hash* of the caller-supplied key against a set of
known-good hashes (a single row via SQL equality, or Go map membership for
the env-var fallback), never the raw key itself. A timing side channel here
would at most reveal information about which pre-existing hash the request's
hash happened to collide with — it does not let an attacker recover bytes of
the *raw* secret key, because the attacker never controls or observes a
byte-by-byte comparison against their own input. This is different from
comparing a raw secret directly (see below), which is why the DB/env-var
lookups intentionally do not use `subtle.ConstantTimeCompare`.

### Constant-time comparison for directly-compared secrets

Two endpoints compare a caller-supplied header directly against a single
configured secret, rather than against a hash set — this is the shape of
comparison a timing attack can actually exploit, byte by byte, against the
live secret:

- `X-Admin-Key` (admin endpoints: `GET/PATCH/DELETE /v1/api-keys*`,
  `GET /v1/admin/db`, `GET /v1/admin/keys/{id}/usage`, contract admin routes)
- `X-Internal-Key` (`GET /internal/status`)

Both are checked via `handlers.validAdminKey`, which wraps
`crypto/subtle.ConstantTimeCompare` and additionally rejects an empty
provided value up front (`services/api/handlers/admin.go`). No comparison of
these secrets anywhere in `services/api` uses `==`, `strings.Compare`, or
`bytes.Equal` (none of which are constant-time — `bytes.Equal` in particular
short-circuits on length and often on early byte mismatches).

Note: this repo does not attempt to *prove* constant-time behavior via
statistical timing measurements in a unit test — scheduler jitter, GC pauses,
and CPU frequency scaling make that approach unreliable and flaky in CI.
Instead, `handlers.TestValidAdminKey_TableDriven` proves the comparator's
*correctness* (equal, mismatched, and different-length inputs all resolve as
expected), which is what verifies `validAdminKey` is wired correctly around
`subtle.ConstantTimeCompare` rather than around a shortcut that would
reintroduce a timing leak.

### Raw keys never reach logs, errors, or the audit trail

- Error responses (`internal/httputil.WriteErrorCtx`) only ever emit a fixed
  `{code, message, request_id}` body — no request header or variable holding
  a raw key is ever interpolated into an error message anywhere in
  `services/api`.
- The audit log (`middleware.AuditWriter` / `middleware.AuditMiddleware`)
  only records `api_key_id` (the opaque UUID resolved *after* successful
  auth), endpoint, method, IP, user agent, status code, duration, and
  request id — it never reads or persists `X-API-Key`, `X-Admin-Key`, or
  `X-Internal-Key`.
- `middleware.TestNoRawKeyLeakage` and `handlers.TestInternalStatus_NoRawKeyLeakage`
  send a distinctive, known raw key through the real middleware stack (auth
  failure and success paths) and assert that value is absent from the
  response body, the structured logger output, and the queued audit entry.

# Security Policy

Trident indexes on-chain Soroban event data and issues API keys that gate
access to it. We take reports of security issues seriously and appreciate
the effort of anyone who helps us find and fix them responsibly.

## Reporting a Vulnerability

**Please do not open a public GitHub issue for a security vulnerability.**

Report it privately via
[GitHub Security Advisories](https://github.com/Telocel-Labs/Trident/security/advisories/new)
for this repository — this opens a private draft advisory visible only to
the maintainers until it's ready to be disclosed. This is the preferred and
fastest path for us to triage a report.

If you cannot use GitHub Security Advisories for some reason, open a regular
issue asking a maintainer to reach out over a private channel and omit
exploit details from the public issue itself.

When reporting, please include as much of the following as you can:

- A description of the vulnerability and its potential impact.
- Steps to reproduce, or a proof-of-concept.
- The affected component(s) — e.g. `services/api`, `crates/indexer`,
  `crates/api`, `docker/nginx`, a specific SDK.
- Any relevant logs, request/response examples, or configuration.

## Scope

**In scope:**

- The Go REST API (`services/api`)
- The Rust gRPC API and indexer (`crates/api`, `crates/indexer`, `crates/backfill`, `crates/common`)
- The WebSocket/GraphQL-over-WS subscription endpoints (`/ws`, `/graphql`)
- The nginx edge configuration (`docker/nginx`)
- The Helm chart and Kubernetes deployment manifests (`helm/`)
- The client SDKs (`sdk/go`, `sdk/typescript`, `sdk/react`, `sdk/python`)
- The reference contracts under `contracts/` as used by this project's own
  CI/test harness

**Out of scope:**

- Vulnerabilities in third-party dependencies that are already tracked
  upstream — please report those to the upstream project, though a heads-up
  here is still welcome so we can track our own exposure (see
  `docs/security-triage.md`).
- Denial-of-service findings that rely purely on volumetric traffic against
  a self-hosted deployment with no rate limiting or reverse proxy configured
  — deploy behind the provided nginx config (or equivalent) and with the
  rate-limiting middleware enabled (`services/api/middleware/ratelimit.go`,
  `abuse.go`).
- Issues that require an attacker to already possess a valid admin API key
  or database credentials (that's the trust boundary, not a vulnerability
  in it — see `docs/threat-model.md`).
- Social engineering, physical access, or attacks against GitHub/CI
  infrastructure itself rather than the Trident codebase.
- A full penetration test or automated scanner report with no manual
  triage — we welcome these as informal input but they don't need a private
  advisory unless a specific, validated finding is included.

## What to Expect

- **Acknowledgement:** we aim to acknowledge a new report within 5 business
  days.
- **Triage:** we aim to have an initial severity assessment and next steps
  within 10 business days of acknowledgement.
- **Fix & disclosure:** timeline depends on severity and complexity. We
  practice coordinated disclosure — we'll work with you on a disclosure
  timeline once a fix is available or ready to ship, and will credit
  reporters (unless you prefer to stay anonymous) in the advisory when it's
  published.

This is a small, MVP-stage open-source project without a dedicated security
team or a bug bounty program at this time. We will do our best to meet the
timelines above, but please be patient — and thank you for helping keep
Trident and its users safe.

## Supported Versions

Trident does not yet have a stable release line (see the pre-alpha status in
[`README.md`](./README.md)). Security fixes land on the `dev` branch and are
included in the next release; there is no separate backport policy at this
stage.

## Related Documentation

- [`docs/threat-model.md`](./docs/threat-model.md) — assets, entry points,
  trust boundaries, and top threats + mitigations.
- [`docs/security-triage.md`](./docs/security-triage.md) — how dependency
  and image vulnerability scan findings are triaged (`.github/workflows/security.yml`).

## Review Cadence

This policy and the linked threat model are reviewed quarterly, and
whenever a new public entry point, trust boundary, or major dependency is
added to the system — see the "Review cadence" section at the bottom of
`docs/threat-model.md`.

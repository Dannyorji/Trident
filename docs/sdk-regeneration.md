# SDK regeneration and testing

Trident ships five client SDKs — Go, Python, React, Rust, and TypeScript —
generated from one source of truth, `api/openapi.yaml`. This document is the
cross-SDK reference: what's generated vs. hand-written in each, the order to
regenerate them in, how each is tested, and how version consistency is
maintained. Each SDK's own README covers its language-specific regeneration
command and API in more depth; this page exists so a contributor touching
`api/openapi.yaml` knows what to do across all five without reading five
separate READMEs first.

## What's generated vs. hand-written

`scripts/generate_sdk_models.py` generates the request/response model types
for four of the five SDKs directly from `api/openapi.yaml`:

| SDK | Generated file | Command |
|---|---|---|
| Go | `sdk/go/openapi/models_gen.go` | `python3 scripts/generate_sdk_models.py --language go` |
| Python | `sdk/python/src/trident_indexer/openapi_models_gen.py` | `python3 scripts/generate_sdk_models.py --language python` |
| Rust | `sdk/rust/src/openapi_models_gen.rs` | `python3 scripts/generate_sdk_models.py --language rust` |
| TypeScript | `sdk/typescript/src/api-types.gen.ts` | `python3 scripts/generate_sdk_models.py --language typescript` |

`--language all` (or the bare `python3 scripts/generate_sdk_models.py`)
regenerates all four in one pass. Under the hood: Go, Python, and Rust go
through [quicktype](https://quicktype.io) against a JSON Schema wrapper
built from the OpenAPI components; TypeScript goes through
[openapi-typescript](https://github.com/openapi-ts/openapi-typescript)
directly. See the script itself (`scripts/generate_sdk_models.py`) for the
exact transform.

**React has no generated file of its own.** `@trident-indexer/react`
consumes the TypeScript SDK's types directly (`SorobanEvent`,
`QueryEventsParams`, etc., re-exported from `@trident-indexer/sdk`) rather
than generating a parallel copy — see
[`sdk/react/README.md`](../sdk/react/README.md#regenerating-openapi-models).
Regenerating TypeScript's models is what keeps React's types current; there
is no separate React-specific step.

Everything else in every SDK — the client class, retry logic, pagination
helpers, WebSocket subscription handling, webhook signature verification
(Rust), React hooks — is hand-written and is not touched by regeneration.

## Regeneration procedure

1. Change `api/openapi.yaml`.
2. Install the one generator dependency if you haven't:
   `python3 -m pip install PyYAML`.
3. Regenerate all four generated files: `python3 scripts/generate_sdk_models.py`.
4. Review the diff in each of the four generated files — a spec change
   should produce a change in every SDK whose types it affects, not a
   subset. If a change to `api/openapi.yaml` produces a diff in some
   generated files but not others, that's worth investigating before
   committing: either the missed SDK's generator has a gap, or the spec
   change didn't actually affect that language's model layer for a
   reason worth stating in the commit message.
5. Update hand-written code that references changed fields (client
   methods, the React hooks, examples) — regeneration only updates the
   generated model files, not code that consumes them.
6. If the API version in `api/openapi.yaml`'s `info.version` changed, bump
   all five SDKs' version fields to match (see
   [Version consistency](#version-consistency) below) — regeneration does
   not do this for you.
7. Run each SDK's test suite (see [Testing](#testing)) before committing.

CI enforces step 3–4's outcome automatically: the `openapi-lint` job in
`.github/workflows/ci.yml` regenerates all four files fresh and fails the
build if that produces any diff against what's committed
("Generated SDK models are stale. Run
'python3 scripts/generate_sdk_models.py' and commit changes.") — so a
regeneration that wasn't run, or was run and not committed, is caught
before merge, not discovered later by a user hitting a type mismatch.

## Testing

Each SDK has its own test suite that exercises real client behavior, not
just that the generated types compile:

| SDK | Command | What it covers |
|---|---|---|
| Go | `cd sdk/go && go test ./...` | Client, retry, streaming |
| Python | `cd sdk/python && pip install -e ".[dev]" && pytest -q` | Sync + async clients, retry, config |
| Rust | `cargo test -p trident-sdk` (or `cargo test --all` from the repo root) | Client, retry, webhook signature verification (including published test vectors), doc-tests |
| TypeScript | `cd sdk/typescript && npm install && npm run build && npm run test` | Client, retry, pagination iterator, GraphQL, config |
| React | `cd sdk/react && npm install && npm run test` | Hooks (`useContractEvents`, `useSubscription`) against a mocked `TridentClient` |

**React's tests require the TypeScript SDK to be built first.** React
depends on `@trident-indexer/sdk` via `"file:../typescript"` in its
`package.json`, which resolves to that package's `dist/` output — if
`sdk/typescript` was never built (no `dist/`), React's test run fails at
import resolution, not at a test assertion, which can look like a broken
test rather than a missing build step. `make test` runs the TypeScript
build before the React test step for exactly this reason; if you're running
these two SDKs' tests outside of `make test`, build TypeScript first.

`make test` at the repository root runs every SDK's suite in the table
above, plus `cargo test --all` and `services/api`'s Go tests, in one
command — see the `test` target in the root `Makefile`.

## Version consistency

All five SDKs and the API spec itself are versioned together. At the time
of writing, every one of the following is `1.0.0`:

- `api/openapi.yaml`'s `info.version`
- `sdk/go/VERSION`
- `sdk/python/pyproject.toml`'s `version`
- `sdk/rust/Cargo.toml`'s `version`
- `sdk/typescript/package.json`'s `version`
- `sdk/react/package.json`'s `version`

There is no automated check enforcing this consistency — it's currently a
manual convention, verified here by hand rather than by tooling. If a
future spec change bumps the API version, bump all five SDK version fields
in the same PR, and consider adding a CI check (parallel to the
generated-models drift check in `.github/workflows/ci.yml`) that fails if
they diverge, rather than relying on a contributor remembering this
document.

## Publishing

Actual publication (npm, PyPI, crates.io, pkg.go.dev, Go module proxy) is
handled by `.github/workflows/publish-sdk.yml` and
`.github/workflows/release.yml` — this document covers regenerating and
testing locally, not the release pipeline itself.

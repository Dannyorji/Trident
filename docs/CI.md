# CI caching and job layout

This document explains how CI caching is set up (issue #309) so future changes
to `.github/workflows/ci.yml` don't accidentally regress it, and how to verify
cache effectiveness on any given run.

## Rust: sccache + shared rust-cache keys

Every Rust job (`rust`, `rust-integration`, `sdk-rust`, `contracts`,
`e2e-contract-events`) sets:

```yaml
env:
  RUSTC_WRAPPER: sccache
  SCCACHE_GHA_ENABLED: "true"
```

and runs `mozilla-actions/sccache-action` before the `Swatinem/rust-cache`
step. `sccache` caches individual compiled objects (keyed by crate, flags,
and rustc version) in the GitHub Actions cache backend, rather than only
caching a job-local `target/` directory. That makes the cache reusable
*across* jobs that compile overlapping code, not just across re-runs of the
same job:

- `rust` and `rust-integration` build the exact same root workspace against
  the same `Cargo.lock`. They now share one `Swatinem/rust-cache` key
  (`shared-key: rust-workspace`) plus the sccache object cache. These two
  jobs have no `needs:` between them and run in parallel, so on a run where
  the cache is cold, both build independently and only one of them wins the
  cache save at end-of-job (GitHub Actions cache keys are immutable — the
  second write is a harmless no-op). The payoff is therefore mostly a
  cross-run one: once either job has populated `rust-workspace`, subsequent
  runs of both jobs reuse it, rather than each job maintaining its own
  separate, job-scoped cache as before.
- `contracts` and `e2e-contract-events` both compile the `token` contract to
  `wasm32v1-none`. `e2e-contract-events` has `needs: [contracts, ...]`, so by
  the time it runs, `contracts` has already populated the shared
  `contracts-wasm` cache key — the second build becomes a near-total cache
  hit instead of a full wasm rebuild.

Each job ends with a `sccache --show-stats` step (`if: always()`) so cache
hit/miss counts for that run are visible directly in the job log without
needing a separate dashboard.

`cargo fmt --all -- --check` in the `rust` job was also moved to run
immediately after toolchain setup, before the Postgres-dependent `sqlx-cli`
install / migrate / `sqlx prepare` steps. Formatting is the most common CI
failure and needs neither the database nor a compiled dependency graph, so it
now fails in seconds instead of after several minutes of DB setup.

## Docker: per-image cache scopes

The `docker`, `e2e`, and `e2e-contract-events` jobs build/restore three
images (Go API, Rust gRPC API, Rust indexer) using
`docker/build-push-action` with `cache-from`/`cache-to: type=gha`.

Previously none of these specified a `scope`, which means the `gha` cache
backend defaulted all three builds to the same `buildkit` scope. Each image
build's cache write was overwriting the previous image's cache blobs for that
scope, so effective hit-rate degraded as more images were added to the
workflow. Each image now uses its own scope (`scope=go-api`, `scope=grpc-api`,
`scope=indexer`) on both the write side (`docker` job) and every read side
(`e2e`, `e2e-contract-events`), so the three caches accumulate independently
instead of colliding.

The Dockerfiles themselves (`services/api/Dockerfile`,
`crates/api/Dockerfile`, `crates/indexer/Dockerfile`) already copy manifest
files (`go.mod`/`go.sum`, `Cargo.toml`/`Cargo.lock`) and build a
dependency-only stub before `COPY . .`, so dependency-fetch layers stay
cached across changes that only touch application source. That ordering was
already correct and is unchanged here.

## Fast feedback vs. heavy jobs

- A workflow-level `concurrency` group cancels superseded runs on the same
  branch/PR, so pushing a fixup commit doesn't leave a stale, doomed run
  burning a runner for another 10+ minutes.
- `rust`, `go`, `typescript`, `openapi`, `sdk-*`, and `contracts` run in
  parallel with no `needs`, so lint/unit feedback isn't serialized behind the
  heavy jobs.
- `e2e` and `e2e-contract-events` (full Docker Compose stacks, and for the
  latter, a local Soroban network) are gated behind `needs:` on the jobs that
  produce their inputs (images, contract WASM), so they only start once
  there's a reasonable chance the fast checks will also pass, and they never
  block the fast jobs from reporting first.

## Verifying the improvement on a PR

There's no static "before/after" number to hardcode here, since actual
wall-clock depends on GitHub's shared-runner load at the time. To verify on a
real PR:

1. Compare the `rust-integration` job's cache-setup and build step durations
   against a `rust` job run in the same workflow run — after the first run
   populates the `rust-workspace` sccache/rust-cache entries, `sccache
   --show-stats` in `rust-integration` should show a high hit count.
2. Compare `contracts` vs. `e2e-contract-events`'s "Build the reference token
   contract" step duration — the latter should be dramatically shorter once
   the `contracts-wasm` cache is warm.
3. In the `docker`, `e2e`, and `e2e-contract-events` job logs, look for
   `CACHED` markers on the Dockerfile steps prior to `COPY . .` — these
   should now hit consistently across runs that don't change dependency
   manifests, instead of only on the specific job that most recently wrote
   the shared `buildkit` scope.

## Coverage: collection and enforced floors

The `coverage` job collects coverage for Rust, Go, and the TypeScript/Python
SDKs, publishes a summary to the workflow run, uploads the raw reports as the
`coverage-reports` artifact (14-day retention), and enforces a floor on the
MVP-critical packages (issue #325).

### Running it locally

```bash
make coverage          # collect everything
make coverage-rust     # Rust only  -> target/llvm-cov/html/index.html
make coverage-go       # Go only    -> services/api/coverage.html
make coverage-sdk      # TypeScript + Python SDKs
make coverage-check    # enforce the same floors CI enforces
```

Rust coverage needs `cargo-llvm-cov`:

```bash
cargo install cargo-llvm-cov
rustup component add llvm-tools-preview
```

`cargo llvm-cov` is used rather than `cargo tarpaulin` because it drives the
compiler's own instrumentation, so it reports the regions rustc actually
generates instead of a ptrace-based approximation.

### Why the floors are where they are

Thresholds are set from **measured** baselines, never from aspiration. A floor
above the current number turns the next unrelated merge red, which trains people
to bypass the gate. The floors sit just under each package's real figure: they
catch regression while leaving room for normal variation.

Measured on the commit that introduced this job:

| Package | Measured | Floor |
|---|---|---|
| `services/api/handlers` | 45.5% | 43% |
| `services/api/middleware` | 69.6% | 66% |
| `services/api/cursor` | 94.4% | 90% |
| `services/api/validation` | 99.2% | 95% |
| Python SDK (hand-written client) | 84.0% | 75% |

Raise them deliberately as suites grow — that ratchet is the point of the gate.

Two deliberate exclusions:

- **Rust is report-only for now.** No baseline existed before this job, and
  picking a threshold without one is the guesswork this section argues against.
  The first runs publish the real figure; enforce from that in a follow-up.
- **The Python SDK floor covers hand-written code only.** `openapi_models_gen.py`
  is emitted by `scripts/generate_sdk_models.py`; "cover the generator's output"
  is not a meaningful ask of a test suite. Including it drags the reported total
  to ~53% and would fail the build for a reason no test can fix. The
  `fail_under = 90` in `sdk/python/pyproject.toml` is aspirational and is
  overridden in CI with `--cov-fail-under=0`; the enforced gate is the
  exclusion-aware check in the workflow.

Coverage is deliberately scoped to critical packages rather than the whole tree,
per the issue's explicit non-goal of a repo-wide coverage mandate.

## Schema guard: migration lint and schema.sql drift

The `schema-guard` job runs two checks on every PR (issue #436, closes #246).

### Why

Migration 0017 renamed `soroban_events` to `soroban_events_legacy` and then ran
`CREATE INDEX IF NOT EXISTS` under the original index names. Index names are
unique per schema, not per table, and those names still belonged to the legacy
table's own indexes — so every one of those statements silently no-op'd. Step 8
then dropped the legacy table, taking the indexes with it. `schema.sql` still
listed all six, nothing compared the two, and the database ran without them for
nine migrations until #437 measured it.

Both checks below fail on the commit that introduces that class of mistake.

### `scripts/lint-migrations.sh`

Static, no database required. Rules:

| Rule | Catches |
|---|---|
| numbering | Duplicate version prefixes (sqlx keys `_sqlx_migrations` by them, so a duplicate aborts a run part-way) and gaps, which usually mean a migration was lost in a rebase |
| `destructive` | `DROP`/`TRUNCATE` without `IF EXISTS` — the 0017 pattern |
| `no-guard` | `CREATE` without `IF NOT EXISTS`, so a partially-applied migration can be re-run |
| `long-lock` | `CREATE INDEX` without `CONCURRENTLY` on a large table, and `ADD COLUMN NOT NULL` without a `DEFAULT` (rewrites the table) |

Waive a rule with a comment on the line above the statement, naming the reason:

```sql
-- lint:allow-destructive the legacy table's rows were copied in step 5
DROP TABLE soroban_events_legacy;
```

Waivers are deliberately noisy to write. Removing data or taking a production
lock should be a decision someone recorded, not a default.

Existing migrations carry waivers where the pattern is genuinely safe — 0017
runs inside `BEGIN/COMMIT`, where `CREATE INDEX CONCURRENTLY` is not even legal,
and builds its indexes on an empty table.

### `scripts/check-schema-drift.sh`

Builds two schemas in a scratch database — one from the migration chain, one
from `schema.sql` — introspects both, and diffs the result.

The comparison is structural (columns with types and nullability, indexes,
constraints), not textual: `pg_dump` output reorders and reformats between
server versions, which produces failures that say nothing about the schema.

Two things are normalised, both artifacts rather than differences:

- **Partition children** (`soroban_events_p*`) are excluded. They are created by
  `create_soroban_partition()`, so requiring a documentation file to enumerate
  them would be busywork.
- **`soroban_events_new_*` constraint names** are rewritten to
  `soroban_events_*`. Migration 0017 created the table under a temporary name
  and renamed it, and PostgreSQL does not rename a table's constraints with it.

Run it locally against a scratch database:

```bash
export DATABASE_URL=postgres://postgres:trident@localhost:5432/scratch
bash scripts/check-schema-drift.sh
```

It creates and drops schemas in that database, so point it at something
disposable.

### What this found

Adding the check immediately surfaced three real defects in `schema.sql`, all
fixed in the same change:

1. It could not be applied to an empty database at all — a trigger on
   `webhook_subscriptions` was declared before the table that owns it.
2. Nine tables added by migrations 0010–0023 were missing entirely
   (`token_events`, `contract_specs`, `token_metadata`, and six others).
3. `webhook_deliveries` was missing the `status` and `attempts` columns from
   migration 0013, and `soroban_events` was missing the natural-key constraint
   from 0025.

`schema.sql` is a dev-bootstrap and documentation convenience — CI and
production apply the migration chain — but `make migrate` falls back to it when
`sqlx-cli` is absent, so a developer bootstrapping from it was getting a
materially different database from production.

## SDK versioning and publishing

### Versions are tied to the spec

All five SDKs (TypeScript, React, Python, Rust, Go) declare the version of the
`api/openapi.yaml` spec they are generated from. `scripts/check-sdk-versions.sh`
enforces this in the `schema-guard` job.

The rule exists because the SDKs are generated artifacts. If `sdk/python` ships
`0.3.0` built from spec `1.1.0` while `sdk/typescript` ships `0.2.0` built from
`1.0.0`, the version tells a user nothing about which API contract the client
implements. Tying them together means `trident-sdk 1.2.0` implements OpenAPI
`1.2.0`, and a breaking spec change bumps all five at once.

Realign after a spec version bump:

```bash
scripts/check-sdk-versions.sh --fix
```

Go has no version field in `go.mod` — the module proxy resolves versions from
git tags — so `sdk/go/VERSION` records the intended version in-tree, and the
publish job refuses to tag unless it matches.

### Publishing

`publish-sdk.yml` is `workflow_dispatch` only. Pick an SDK (or `all`), give a
semver version, and optionally check `dry_run`.

| SDK | Registry | Mechanism |
|---|---|---|
| TypeScript | npm | `npm publish --provenance` |
| React | npm | same, after rewriting the `file:../typescript` dependency to the published range |
| Python | TestPyPI → PyPI | trusted publisher (OIDC), promotion gated on the `pypi` environment |
| Rust | crates.io | `cargo publish`, always preceded by `--dry-run` |
| Go | module proxy | git tag `sdk/go/vX.Y.Z`, then a proxy warm request |

`dry_run` builds, tests, and packages everything without publishing. Use it
before a real release: it exercises the parts that usually break — a Rust crate
that compiles in-workspace but not standalone, or a React package whose
`file:` dependency cannot resolve outside this repo.

Two things are deliberately not automated:

- **Publishing is never triggered by a push.** Registry uploads are effectively
  irreversible (npm unpublish is time-limited, crates.io has none, Go module
  versions are immutable), so a human chooses the moment.
- **The Go publish creates a tag.** Tags are the module version, so republishing
  a version is impossible by design — the job fails rather than trying.

### Credentials

`NPM_TOKEN` and `CARGO_REGISTRY_TOKEN` are repository secrets. Python uses
PyPI's trusted-publisher OIDC flow, so it has no token to manage. The Go job
uses the workflow's own `GITHUB_TOKEN` to push a tag.

A first publish of any package also needs the name to be claimed on the registry
and, for Python, the trusted publisher configured for this repo and workflow.
That is a one-time manual setup per registry and cannot be done from CI.

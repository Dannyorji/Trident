# CI layout, caching and job structure

This document explains how the CI workflows are organised, how a pull request
is gated, and how caching is set up (issue #309) so future changes don't
accidentally regress either.

## Workflow layout

There are six workflows in `.github/workflows/`. Shared setup lives in
composite actions under `.github/actions/` so the workflow files stay short.

| Workflow | Trigger | What it holds |
|---|---|---|
| `ci.yml` | PR, push to dev/main | Path-filtered lint + unit tests (Rust, Go, every SDK, explorer build/boot/a11y, contracts) and static checks (OpenAPI lint + generated-model drift, Prometheus rules + promtool, Alertmanager routing, Helm, schema guard, env reference, migration versions, image digests, docs examples + links, actionlint). Gate: **`ci-ok`**. |
| `integration.yml` | PR, push to dev/main | Image build (once, shared as artifacts), Rust integration tests with Postgres/Redis, E2E + OpenAPI contract test, E2E contract events, quickstart verification, coverage. Gate: **`integration-ok`**. |
| `security.yml` | PR, push, nightly, dispatch | Gitleaks, dependency audits (cargo/govulncheck/npm/pip), SBOM + image scan of the images `integration.yml` built, migration image secret audit. Gate: **`security-ok`**. |
| `nightly.yml` | schedule, dispatch | Testnet ingest correctness, explorer a11y + perf budget, launch gate checker, launch readiness, backup/restore drill, and one failure notification for all of them. |
| `deploy.yml` | push to dev (staging); release or dispatch (production) | One pipeline parameterised by environment: config check, migrate + rollout (Helm), smoke test, rollback, alert. |
| `release.yml` | `vX.Y.Z` tag, dispatch | Requires green `ci-ok`/`integration-ok`/`security-ok` on the tagged commit, builds + signs + attests the images, publishes every SDK, creates the GitHub release. |

### Required checks

Branch protection on `dev` and `main` requires exactly three checks:
`ci-ok`, `integration-ok` and `security-ok`. Each is an aggregate job that
`needs:` every other job in its workflow and fails if any of them **failed or
was cancelled**; a **skipped** job (filtered out by paths) counts as success.
Adding, renaming or removing a job therefore changes only that aggregate's
`needs:` list, never the required-check list -- which is what stops a renamed
job from silently dropping out of the required set.

`scripts/set-branch-protection.sh` applies this to both branches (needs repo
admin; it only updates the required-status-checks part of existing protection).

### Path filters

`ci.yml`, `integration.yml` and `security.yml` each start with a `changes` job
that runs `dorny/paths-filter` against one shared file,
`.github/path-filters.yml`. On a pull request a job runs only if its filter
matched; on a push to dev/main (and schedule/dispatch) every filter is forced
true, so the post-merge run is always the full suite. Editing a workflow, a
composite action or the filters file re-runs everything in the workflows that
use it.

In practice: a PR touching only `docs/site/**` runs `changes`, the always-on
static checks, the `docs` job and the aggregates (minutes); a PR touching
`crates/` runs the full Rust, integration and E2E set.

### Adding a job

1. Add the job to the right workflow, with `needs: changes` and
   `if: needs.changes.outputs.<filter> == 'true'` if it should be path-filtered
   (add the filter to `.github/path-filters.yml` and to the `changes` job's
   outputs).
2. Add it to the `needs:` list of that workflow's aggregate job. That is the
   only step that makes it gating.
3. Do not touch branch protection.

### Composite actions (`.github/actions/`)

| Action | Used for |
|---|---|
| `setup-rust` | toolchain + sccache + rust-cache, wired identically everywhere |
| `setup-go`, `setup-node` | pinned toolchains with optional module/npm cache |
| `build-image` | one image with a per-image gha cache scope (CI, staging push, release push) |
| `get-image` | reuse the image `integration.yml` built for this commit, else build it |
| `wait-for-api`, `stack-logs` | E2E stack readiness and failure diagnostics |
| `explorer-a11y` | the axe-core accessibility run (ci.yml and nightly.yml) |
| `gate` | the aggregate pass/fail logic behind `ci-ok`, `integration-ok`, `security-ok` |

### Linting the workflows

`actionlint` runs in `ci.yml` whenever anything under `.github/` changes. A
malformed workflow file (the old `ci.yml` had a duplicate `explorer` job key
that disabled all 21 of its jobs unnoticed, #708) now fails the PR that
introduces it. Its shellcheck/pyflakes integrations are off so the gate is not
blocked by pre-existing findings in inline scripts.

## Rust: sccache + shared rust-cache keys

Every Rust job (`rust`, `sdk-rust`, `contracts` in `ci.yml`;
`rust-integration`, `e2e-contract-events`, `coverage` in `integration.yml`;
`testnet-correctness` in `nightly.yml`) uses the `setup-rust` composite
action, which installs the toolchain, sets `RUSTC_WRAPPER=sccache` and
`SCCACHE_GHA_ENABLED=true`, and runs `mozilla-actions/sccache-action` before
the `Swatinem/rust-cache` step. `sccache` caches individual compiled objects (keyed by crate, flags,
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
- `contracts` (`ci.yml`) and `e2e-contract-events` (`integration.yml`) both
  compile the `token` contract to `wasm32v1-none` with the shared
  `contracts-wasm` cache key. They are separate workflows and run in parallel,
  so the sharing is cross-run: once either has populated the key, the other
  reuses the compiled objects instead of rebuilding the crate.

Each job ends with a `sccache --show-stats` step (`if: always()`) so cache
hit/miss counts for that run are visible directly in the job log without
needing a separate dashboard.

`cargo fmt --all -- --check` in the `rust` job was also moved to run
immediately after toolchain setup, before the Postgres-dependent `sqlx-cli`
install / migrate / `sqlx prepare` steps. Formatting is the most common CI
failure and needs neither the database nor a compiled dependency graph, so it
now fails in seconds instead of after several minutes of DB setup.

## Docker: built once, per-image cache scopes

`integration.yml`'s `build-images` job builds the four images (Go API, Rust
gRPC API, Rust indexer, migrate) exactly once per commit, using the
`build-image` composite action, and uploads each as an artifact
(`image-<name>`, 1-day retention). Everything that needs an image consumes
that artifact instead of rebuilding:

- `e2e` and `e2e-contract-events` `docker load` the images and run
  `docker compose up --no-build`. `docker/docker-compose.ci.yml` pins each app
  service to the `:ci` tag `build-images` produces.
- `security.yml` scans the same images. The two workflows run in parallel, so
  `scripts/fetch-image-artifact.sh` waits for the artifact. If `integration.yml`
  was path-filtered out, failed before the build, or this is a nightly/manual
  run, the `get-image` action builds the image itself.
- `deploy.yml` and `release.yml` build their own (registry-pushed, and for
  release multi-arch) images with the same `build-image` action.

Every image uses its own gha cache scope (`go-api`, `grpc-api`, `indexer`,
`migrate`). Without an explicit `scope` the `gha` backend puts every build in a
workflow into one shared `buildkit` scope, and the builds overwrite each other's
cache blobs on every run. The same scope names are used by every workflow, so
the cache accumulates across them.

The Dockerfiles themselves (`services/api/Dockerfile`,
`crates/api/Dockerfile`, `crates/indexer/Dockerfile`) already copy manifest
files (`go.mod`/`go.sum`, `Cargo.toml`/`Cargo.lock`) and build a
dependency-only stub before `COPY . .`, so dependency-fetch layers stay
cached across changes that only touch application source.

## Fast feedback vs. heavy jobs

- A workflow-level `concurrency` group cancels superseded runs on the same
  branch/PR, so pushing a fixup commit doesn't leave a stale, doomed run
  burning a runner for another 10+ minutes.
- Lint and unit tests (`ci.yml`) are a separate workflow from the heavy
  checks (`integration.yml`), so they report first and run in parallel with
  them; neither workflow waits on the other.
- Within `integration.yml`, `e2e` and `e2e-contract-events` (full Docker
  Compose stacks, and for the latter a local Soroban network) `need` only
  `build-images`, so they start as soon as the images exist.
- Path filters (above) keep a docs-only or explorer-only PR off the Rust/Go
  build, E2E and coverage jobs entirely.

## Verifying the improvement on a PR

There's no static "before/after" number to hardcode here, since actual
wall-clock depends on GitHub's shared-runner load at the time. To verify on a
real PR:

1. Compare the `rust-integration` job's cache-setup and build step durations
   (`integration.yml`) against the `rust` job (`ci.yml`) on the same commit — after the first run
   populates the `rust-workspace` sccache/rust-cache entries, `sccache
   --show-stats` in `rust-integration` should show a high hit count.
2. Compare `contracts` vs. `e2e-contract-events`'s "Build the reference token
   contract" step duration — the latter should be dramatically shorter once
   the `contracts-wasm` cache is warm.
3. In the `build-images` job logs, look for
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

The `schema-guard` job runs on every PR that touches `database/`, `api/` or `sdk/` (issue #436, closes #246).

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

SDKs are published by `release.yml`: every SDK on a `vX.Y.Z` tag, or via
`workflow_dispatch` to publish one SDK (or `all`) at a semver version of your
choice, optionally with `dry_run`.

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

- **Publishing is never triggered by an ordinary push.** Registry uploads are
  effectively irreversible (npm unpublish is time-limited, crates.io has none,
  Go module versions are immutable), so the only triggers are a deliberate
  `vX.Y.Z` tag -- which `release.yml` refuses to act on unless `ci-ok`,
  `integration-ok` and `security-ok` are green for that commit -- or a manual
  dispatch.
- **The Go publish creates a tag.** Tags are the module version, so republishing
  a version is impossible by design — the job fails rather than trying.

### Credentials

`NPM_TOKEN` and `CARGO_REGISTRY_TOKEN` are repository secrets. Python uses
PyPI's trusted-publisher OIDC flow, so it has no token to manage. The Go job
uses the workflow's own `GITHUB_TOKEN` to push a tag.

A first publish of any package also needs the name to be claimed on the registry
and, for Python, the trusted publisher configured for this repo and workflow.
**PyPI and TestPyPI trusted publishers pin the workflow file name**: they must
be pointed at `release.yml` (they previously pointed at `publish-sdk.yml`)
before the next publish.
That is a one-time manual setup per registry and cannot be done from CI.


## Deploys

`deploy.yml` is one pipeline for both environments (`plan` resolves release
name, namespace, values file, image tag and URL once). Staging deploys every
push to `dev` with images built in the workflow; production deploys a release
tag whose signed images `release.yml` already pushed.

The `deploy` and `rollback` jobs run in the GitHub environment named after the
target. Configure **required reviewers** on the `production` environment
(Settings -> Environments) so every production deploy waits for approval
(#782); the rollback job shares the environment, so its protection rules apply
to it too. Secrets/variables: `STAGING_*` as before, plus `PRODUCTION_KUBECONFIG`,
`PRODUCTION_DATABASE_URL`, `PRODUCTION_URL`, `PRODUCTION_API_KEY`,
`PRODUCTION_NAMESPACE` (optional) and `PRODUCTION_ALERT_WEBHOOK_URL`
(optional). A release published by `release.yml` itself does not trigger
`deploy.yml` (GitHub does not chain workflows off `GITHUB_TOKEN` events); run
it via `workflow_dispatch` with `environment=production` and the release tag.

## Releases

`release.yml` on a `vX.Y.Z` tag: `gate` waits for `ci-ok`, `integration-ok` and
`security-ok` on the tagged commit (#781) -> `build-images` pushes multi-arch
images, signs each keylessly with cosign and attaches a SLSA provenance
attestation (#780) -> every SDK is published -> the GitHub release is created
with the image digests. Verify an image with:

```bash
cosign verify \
  --certificate-identity-regexp 'https://github.com/<org>/<repo>/.github/workflows/release.yml@.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/<org>/trident-go-api@<digest>
```

## Nightly

`nightly.yml` runs once a day (06:17 UTC) and on dispatch. Every job feeds one
`notify-failure` step, which posts to `NIGHTLY_ALERT_WEBHOOK_URL` if set and
always leaves an error annotation and a run summary. Jobs moved here from
per-PR workflows, and why:

| Job | Was | Reason |
|---|---|---|
| `launch-gate` | `ci.yml` | guards `docs/runbooks` launch checklist parsing; nothing a typical PR touches |
| `backup-restore-drill` | `ci.yml` | exercises `scripts/backup.sh`/`restore.sh` against a scratch DB; slow, and those scripts rarely change |
| `launch-readiness` (+ config check) | `launch-readiness.yml` | needs live staging + secrets (never available to fork PRs); the PR trigger only covered three docs paths |
| `explorer-perf` | `explorer-perf.yml` | already scheduled-only (depends on the live testnet API) |
| `testnet-correctness` | `testnet-correctness.yml` | already scheduled-only |

# Load & Soak Tests

Load, soak, and validation scripts for the Trident API and ingest pipeline
(issue #322). SLO thresholds used throughout are taken directly from
[`docs/slo.md`](../docs/slo.md) — if that doc's numbers change, update these
scripts to match rather than the other way around.

## Prerequisites

- [k6](https://k6.io/docs/get-started/installation/) installed, for every
  `*.js` script here.
- The stack running locally, e.g.:
  ```bash
  docker compose -f docker/docker-compose.yml up -d --build
  ```
  or the CI-equivalent compose overlay (`docker/docker-compose.ci.yml` +
  `.env.ci`) if you want to match what `.github/workflows/load-tests.yml`
  runs.
- For `ingest-soak.sh` specifically: the
  [stellar CLI](https://github.com/stellar/stellar-cli), a local Soroban
  network reachable at `LOCAL_RPC_URL` (e.g. `stellar/quickstart`), and the
  reference token contract built (`cargo build --release --target
  wasm32v1-none -p token` in `contracts/`) — see the header comment in
  `ingest-soak.sh` for the exact prerequisites and how they mirror
  `.github/workflows/ci.yml`'s `e2e-contract-events` job.

## Scripts

| Script | What it tests | Run |
|---|---|---|
| `pgbouncer-validation.js` | PgBouncer connection pooling under 100 concurrent clients (issue #87) | `BASE_URL=http://localhost:3000 k6 run load-tests/pgbouncer-validation.js` |
| `events-load.js` | `GET /v1/events` (list) + `GET /v1/events/{id}` (get) | `BASE_URL=http://localhost:3000 API_KEY=<key> k6 run load-tests/events-load.js` |
| `batch-load.js` | `POST /v1/events/batch` | `BASE_URL=http://localhost:3000 API_KEY=<key> k6 run load-tests/batch-load.js` |
| `stats-load.js` | `GET /v1/stats/indexer`, `GET /v1/stats/contracts` | `BASE_URL=http://localhost:3000 API_KEY=<key> k6 run load-tests/stats-load.js` |
| `stream-load.js` | `GET /v1/events/stream` (SSE) — connect-and-hold under concurrency | `BASE_URL=http://localhost:3000 API_KEY=<key> CONCURRENT_STREAMS=20 HOLD_SECONDS=30 k6 run load-tests/stream-load.js` |
| `rate-limit-concurrency.js` | One-key burst proving the tier limit holds under launch-scale concurrency | `BASE_URL=http://localhost:3000 API_KEY=<key> EXPECTED_LIMIT=100 CONCURRENT_REQUESTS=1000 k6 run load-tests/rate-limit-concurrency.js` |
| `ingest-soak.sh` | Sustained ingest volume (contract mint loop) + Go API / Rust indexer resource sampling over time | `LOCAL_RPC_URL=http://localhost:8000/rpc SOAK_DURATION_SECONDS=1800 ./load-tests/ingest-soak.sh` |
| `launch-soak.sh` | Combined 24-hour launch soak: ingest volume plus events, batch, stats, and SSE load running together (issue #440) | `BASE_URL=https://staging.example.com API_KEY=<key> ./load-tests/launch-soak.sh` |
| `chaos-launch.sh` | Launch chaos verification for Postgres, Redis, and optional RPC faults with before/during/after readiness probes (issue #439) | `BASE_URL=http://localhost:3000 COMPOSE_FILE=docker/docker-compose.yml RPC_SERVICE=<rpc-service> ./load-tests/chaos-launch.sh` |
| `graceful-shutdown-launch.sh` | Rolling SIGTERM verification for API request drain, SSE reconnect behavior, and indexer cursor safety (issue #442) | `BASE_URL=http://localhost:3000 COMPOSE_FILE=docker/docker-compose.yml ./load-tests/graceful-shutdown-launch.sh` |

`API_KEY` is optional for the k6 scripts — every script's checks accept
either `200` or `401`, so they still validate the server doesn't error/crash
under load even without a configured key, though a real key is needed to
exercise the actual data path meaningfully. Set `API_KEY` to a value present
in `API_KEY_HASHES`/`api_keys` for the target environment.

Every script accepts `BASE_URL`. Point it at staging, not just localhost, to
get a capacity number that means something.

`rate-limit-concurrency.js` requires a valid `API_KEY`. Set `EXPECTED_LIMIT`
to that key's configured tier limit; the test fails if the concurrent burst
allows more than that limit, does not reject the excess, or returns any status
other than 200/429. Run it against an otherwise idle key so earlier requests in
the same sliding window do not affect the expected boundary.

## Launch Verification Runs

### Combined launch soak (`launch-soak.sh`)

`launch-soak.sh` is the orchestration harness for the projected-launch soak in
issue #440. It runs the existing k6 read/write/stat/SSE scripts, PgBouncer pool validation, and the ingest
soak keeps contract events flowing. By default, it uses a 24-hour k6 duration
and an 86,400 second ingest duration:

```bash
BASE_URL=https://staging.example.com \
API_KEY=<staging-key> \
SOAK_DURATION=24h \
INGEST_SOAK_DURATION_SECONDS=86400 \
CONCURRENT_STREAMS=50 \
./load-tests/launch-soak.sh
```

Results are written under `load-tests/launch-soak-results/<timestamp>/` with one
log per workload plus `run-metadata.env` and `summary.txt`. Copy the relevant
latency, failure-rate, memory, connection, cursor, and restart observations into
[`docs/performance.md`](../docs/performance.md) after the run.

`stream-load.js` runs a single connect-and-hold iteration per VU, so it only
covers `HOLD_SECONDS` per invocation. The soak harness relaunches it in a loop
until `SOAK_DURATION` elapses; without that, SSE would stop being exercised
minutes into a 24-hour run. `SOAK_DURATION` accepts `24h`, `90m`, or `300s`.

### Rolling shutdown verification (`graceful-shutdown-launch.sh`)

`graceful-shutdown-launch.sh` covers issue #442 by running API read load and SSE
stream load while sending SIGTERM to the API and indexer services:

```bash
BASE_URL=http://localhost:3000 \
COMPOSE_FILE=docker/docker-compose.yml \
DRAIN_SECONDS=30 \
RECOVERY_SECONDS=45 \
./load-tests/graceful-shutdown-launch.sh
```

Results are written under `load-tests/shutdown-results/<timestamp>/`. Use the
ready probes, k6 logs, and service logs to confirm API requests drain, SSE
clients do not hang silently, the indexer exits at a safe cursor boundary, and
Kubernetes termination settings exceed the measured drain time.

The harness asserts a different readiness expectation per service. Terminating
the API should stop it serving, so a `503` or an unreachable endpoint (recorded
as `000`) passes while a `200` fails — that would mean traffic kept arriving
mid-drain. Terminating the indexer leaves the API untouched, so readiness must
stay `200` throughout. Both scenarios also require recovery to `200` within
`RECOVERY_SECONDS`. The script exits non-zero if any assertion fails or any load
generator fails.
### Launch chaos verification (`chaos-launch.sh`)

`chaos-launch.sh` is the fault-injection harness for issue #439. It records
`/v1/ready` before, during, and after each induced dependency fault:

```bash
BASE_URL=http://localhost:3000 \
COMPOSE_FILE=docker/docker-compose.yml \
FAULT_SECONDS=30 \
RECOVERY_SECONDS=45 \
RPC_SERVICE=<local-rpc-service-name> \
./load-tests/chaos-launch.sh
```

The script covers Postgres down/slow, Redis down/evicted, and RPC down/slow when
`RPC_SERVICE` points at a compose-managed RPC service. Results are written under
`load-tests/chaos-results/<timestamp>/`.

The harness asserts the readiness contract rather than only recording it.
`GET /v1/ready` returns 200 only when Postgres, Redis, and the gRPC backend all
pass, and 503 when any check fails, so each scenario checks that:

- readiness reported 503 while the dependency was stopped or paused — a 200
  means the outage went undetected, and a timeout (recorded as `000`) means the
  probe hung instead of failing fast;
- readiness returned to 200 within `RECOVERY_SECONDS` after the dependency came
  back;
- flushing Redis stays 200, because an emptied cache is a miss, not an outage.

The script exits non-zero if any assertion fails and prints the failure count in
`summary.txt`, so it can gate a launch checklist. Still treat data loss, cursor
corruption, or unbounded retry loops seen in the logs as follow-up issues — those
are not asserted automatically.

**Running it against staging** (issue #499): the harness as written targets
`docker compose` service names, so it cannot run unmodified against a
Kubernetes-deployed staging environment — see
[`docs/runbooks/chaos-drill-findings.md`](../docs/runbooks/chaos-drill-findings.md)
for the exact adaptation needed, a full static verification of the harness's
assertions against the real `/v1/ready` contract, and two coverage gaps found
during that review (PgBouncer faults are never induced directly, and the
default `RECOVERY_SECONDS` was never measured against actual recovery time).
## Interpreting Results

### k6 scripts (`events-load.js`, `batch-load.js`, `stats-load.js`, `stream-load.js`)

k6 prints a summary at the end with `thresholds` marked ✓/✗. A threshold
failure means the run breached an SLO from `docs/slo.md`:

- `http_req_duration{scenario:...}: p(95)<500` (or `<1000` for batch) — read
  routes must stay under the SLO 2 p95 target for their class.
- `http_req_failed: rate<0.005` — SLO 3's 99.5% non-5xx availability target,
  applied as a hard bar over the (short) load-test run rather than SLO 3's
  rolling 28-day window.
- `stream_connected: rate>0.95` (stream-load.js only) — at least 95% of
  attempted SSE connections must not be rejected outright (4xx/5xx). This
  isn't a `docs/slo.md` SLO directly (streaming isn't in the read/write
  latency split there) — it's a basic "the server accepts concurrent
  long-lived connections" capacity check.

A failing threshold means: don't ship this build/config to production
without investigating — either it's an actual regression, or the
environment under test (undersized local Docker resources, a cold cache) is
genuinely why, in which case re-run against a more representative
environment before trusting the result either way.

### `ingest-soak.sh`

Writes results to `load-tests/soak-results/<timestamp>/`:

- `mint.log` — raw `stellar contract invoke` output for every mint in the
  loop, plus a line for any invocation that failed.
- `stats.csv` — `docker stats` samples for the Go API and Rust indexer
  containers on `SAMPLE_INTERVAL_SECONDS` intervals: timestamp, container,
  CPU%, memory usage (MB), memory limit (MB), memory%.
- `summary.txt` — the growth-check verdict: average memory usage in the
  first few samples vs. the last few samples, as a percentage change,
  compared against `MAX_GROWTH_PCT` (default 50%).

The script exits non-zero if any monitored container's memory grew beyond
`MAX_GROWTH_PCT` between the first and last sampling windows.

**Limitation, stated plainly:** this is a coarse "no unbounded growth"
check over one run's duration, not a rigorous leak detector. `docker stats`
memory accounting includes page cache and can be noisy; a single soak run
is one data point. A pass doesn't prove there's no slow leak that would only
show up over hours/days; a fail is a signal worth investigating, not
automatically a confirmed leak. For anything beyond an MVP capacity check,
pair this with the Go API's `PPROF_ENABLED` opt-in profiling
(`services/api/internal/profiling/profiling.go`) for heap/goroutine
snapshots, and/or run it for much longer (multi-hour) durations before
trusting the result for a real capacity decision.

If `GO_API_CONTAINER`/`INDEXER_CONTAINER` don't match your compose project's
actual container names (they default to the `docker compose` project-name
convention, e.g. `docker-api-1`), override them — check with
`docker compose ps` first.

## CI

`.github/workflows/load-tests.yml` runs these scripts against the same
docker-compose stack CI's `e2e` job uses, but only via `workflow_dispatch`
and a weekly `schedule:` cron — never on every PR (issue #322 explicitly
scopes performance-regression gating on every PR out). Trigger it manually
from the Actions tab, or wait for the scheduled run, to get a fresh capacity
read.

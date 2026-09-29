# Chaos drill findings (issue #499)

## Status: harness verified, staging run not performed

Issue #499 asks for `load-tests/chaos-launch.sh` (built for #439) to be run
against staging for Postgres, Redis, and RPC faults, with real observed
degradation behavior recorded. That could not be done from this environment:

- Staging is a Kubernetes deployment reached only through CI secrets
  (`STAGING_KUBECONFIG`, `STAGING_URL`, `STAGING_DATABASE_URL` — see
  `.github/workflows/staging-deploy.yml`), none of which are available here.
- The harness itself is compose-native (`docker compose stop/pause/exec`
  against `docker/docker-compose.yml`'s service names), so it cannot target a
  Kubernetes staging deployment as written without a rewrite to `kubectl`
  equivalents (see "Running against Kubernetes staging" below) — it is built
  and documented for a compose-backed environment.
- A local compose dry run was also not possible: this environment has the
  Docker CLI but no reachable Docker daemon (`docker info` fails to connect
  to the daemon socket), so even `docker compose up` against the existing
  local `docker/docker-compose.yml` could not be exercised here.

Rather than claim a run that didn't happen, this documents what was actually
done: a full static verification of the harness's logic against the real
`/v1/ready` contract and the production compose topology, which surfaced two
genuine gaps worth filing as follow-ups, plus the exact procedure for someone
with staging or local Docker access to execute the real drill.

## What was verified

1. **Shell correctness.** `bash -n load-tests/chaos-launch.sh` and
   `shellcheck load-tests/chaos-launch.sh` both pass clean — no syntax errors,
   no shellcheck warnings.

2. **Readiness contract cross-check.** Read
   `services/api/handlers/health.go` (`Ready` handler) directly against the
   harness's assertions:
   - `GET /v1/ready` returns `503` if any of Postgres, Redis, or the gRPC
     backend check fails, `200` only if all three pass. `expect_degraded`
     (harness line 62) and `expect_healthy` (line 72) match this exactly:
     503 during a fault is a pass, 200 during a fault is a failure, and a
     hang (curl's `000` from `--max-time 10`) is a failure either way rather
     than being mistaken for either state.
   - Redis is checked with a plain `Ping` (`checkRedis`, health.go:186) with
     no dependency on cached keys existing, so `FLUSHDB` correctly leaves
     `/v1/ready` at 200 — the harness's `run_redis_evicting` scenario
     (chaos-launch.sh:124) asserting `expect_healthy` "during" the flush,
     not `expect_degraded`, matches the handler's actual behavior rather than
     the more intuitive-but-wrong assumption that clearing the cache should
     look unhealthy.
   - No RPC/gRPC dependency is reachable from a stubbed local run, so the
     gRPC-down path (`checkGRPC`, health.go:193, a real `ListEvents` call)
     was reasoned through code rather than exercised.

3. **Compose topology cross-check** (`docker/docker-compose.yml`). This is
   where the two findings below came from — see "Findings to file as
   follow-up issues".

## Findings to file as follow-up issues

### Finding 1 — the harness never faults PgBouncer itself

Every service (`indexer`, `api`) connects to `pgbouncer:6432`, never to
`postgres:5432` directly (`docker-compose.yml:91,129`, both comments say
"Route through PgBouncer, never postgres directly"). `chaos-launch.sh`'s
`POSTGRES_SERVICE` defaults to `postgres` and every Postgres scenario
(`postgres-down`, `postgres-slow`) stops or pauses the `postgres` container,
which is one hop upstream of what the application actually talks to.

This is a real gap, not just a naming nit: PgBouncer sits in front of
Postgres and is responsible for detecting a dead backend and either failing
fast or queuing, depending on its pool state — behavior the current
scenarios never exercise. `/v1/ready` failing when `postgres` is stopped only
proves PgBouncer eventually surfaces the outage to `checkPostgres`'s `Ping`;
it says nothing about how long PgBouncer takes to notice, whether it queues
client connections against a dead backend (which would show up as `/v1/ready`
hanging past `--max-time 10` rather than failing fast, indistinguishable in
the harness's output from a genuine timeout), or what happens if `pgbouncer`
itself is stopped/paused instead of the Postgres behind it — a distinct
failure mode (`api`/`indexer` lose their pooler, not their database) that has
no scenario at all today.

**Suggested follow-up**: add a `pgbouncer-down`/`pgbouncer-slow` scenario
pair (stop/pause the `pgbouncer` service directly, same before/during/after
probe shape as the existing scenarios), and consider adding a
`PGBOUNCER_SERVICE` variable alongside `POSTGRES_SERVICE` so the existing
Postgres scenarios can optionally be run against the pooler layer too.

### Finding 2 — recovery timing assumption doesn't account for PgBouncer's reconnect behavior

`RECOVERY_SECONDS` defaults to 45s, and Postgres's own compose healthcheck
needs up to `start_period(15s) + retries(10) × interval(10s)` ≈ 115s in the
worst case to report `healthy` again after a restart
(`docker-compose.yml:13-18`). The harness's 45s default assumes the
application-level recovery (PgBouncer reconnecting to Postgres, `checkPostgres`
succeeding again) happens well before Postgres's own healthcheck would
declare it healthy — which is plausible (PgBouncer doesn't wait on compose's
healthcheck; it retries its own backend connection independently) but was
never actually measured, only assumed by the person who wrote #439's harness
and reused here.

**Suggested follow-up**: when the harness is actually run (staging or local
compose with a working daemon), record the wall-clock time from
`postgres-down-during` to the first `postgres-down-after` probe that returns
200, and if it's ever close to or past `RECOVERY_SECONDS`, either raise the
default or make it configurable per-scenario rather than one shared value for
Postgres, Redis, and RPC recovery.

## Running the real drill (for whoever has staging or local Docker access)

### Against local compose (once a Docker daemon is reachable)

```bash
docker compose -f docker/docker-compose.yml up -d --build
BASE_URL=http://localhost:3000 \
COMPOSE_FILE=docker/docker-compose.yml \
FAULT_SECONDS=30 \
RECOVERY_SECONDS=45 \
./load-tests/chaos-launch.sh
```

There is no local RPC container in `docker/docker-compose.yml`, so
`RPC_SERVICE` will be unset and the rpc-down/rpc-slow scenarios will be
skipped with the harness's own printed instructions for exercising them at
the network/provider layer instead (chaos-launch.sh:149-158).

### Against staging

The harness as written targets `docker compose` service names and cannot run
against a Kubernetes deployment unmodified. Two options, in order of
preference:

1. **Point `BASE_URL` at staging, induce faults at the Kubernetes layer.**
   Keep the harness's probe/assert logic (`probe`, `expect_degraded`,
   `expect_healthy`) but replace the `compose stop/pause/exec` calls with
   `kubectl scale --replicas=0` (Postgres/Redis down),
   `kubectl exec ... -- redis-cli FLUSHDB` (Redis eviction), or a
   network-policy/`kubectl exec ... -- tc` byte-delay for the "slow" variants.
   This preserves the exact readiness assertions this script already encodes
   against `/v1/ready`'s real contract, which is the part worth reusing.
2. **Run `docker compose` directly against staging's Postgres/Redis
   connection strings** if staging genuinely runs the same compose stack
   behind `STAGING_KUBECONFIG` (unclear from this repo alone — the deploy
   workflow uses Helm, which implies Kubernetes, not compose, actually runs
   in staging, making option 1 the realistic path).

Either way: run it, record the produced `load-tests/chaos-results/<run-id>/`
directory's `summary.txt` and `probes.csv` (both gitignored — copy the
relevant numbers into this file or a dated section here rather than the raw
directory), and file every unexpected result as its own issue per the
harness's own printed review checklist and issue #499's "Done when".

# Postgres Restore Drill Runbook

**Status: partially executed.** This documents a real, locally-performed
restore drill (schema/tooling fidelity — see "What this drill proves" below)
with genuine measured numbers. It does **not** replace the production-shaped
drill #501 ultimately asks for, which needs a real staging environment and a
real automated backup to restore from — **neither exists yet** (see
"What's still missing" below). Treat this as the tooling/procedure baseline
the production drill will build on, not as a substitute for it.

## Why #431 blocks the full drill

#501 depends on #431 ("automated Postgres backups with a restore we have
actually performed"), which is **still open**. `grep -rl "backup\|pg_dump\|restore" docs/ scripts/`
confirms there is no backup automation, no cron/CI job producing scheduled
dumps, and no `scripts/backup.sh`/`restore.sh` in this repo. A "production-shaped
backup from staging" (#501's first scope item) cannot be taken because there
is no defined backup artifact or schedule to draw one from yet.

## What this drill proves

Rather than skip #501 entirely while #431 is open, this drill answers the
parts of it that don't require production infrastructure or an existing
backup pipeline: **does `pg_dump`/`pg_restore` actually round-trip this
schema correctly, especially the partitioned `soroban_events` table**, and
**what does a dump/restore of a realistically-sized dataset actually cost in
wall-clock time**. Both are real, useful facts independent of where the
backup schedule eventually lives.

### What was actually done

1. A local PostgreSQL 15.15 instance was started (matching `docker/docker-compose.dev.yml`'s
   pinned `postgres:15-alpine`, verified via `psql -c "select version()"`).
2. The real schema was applied from `database/schema.sql` (the same file
   used for local/dev bootstrap).
3. **Found and fixed a real bug this drill surfaced**: `docker/docker-compose.dev.yml`
   was checked into this repo with corrupted YAML — every double-quoted
   string (`"5432:5432"`, the `healthcheck.test` array) had been mangled
   into literal backslash-newline sequences at some point in its history
   (introduced in `699fce4`, "chore(db): integrate sqlx-cli database
   migration management" / `#93` — confirmed clean at the file's creation
   in `ca79d07`, already corrupted by `699fce4`; `#195` later removed an
   already-corrupted `version:` key without noticing the same corruption
   elsewhere in the file). `python3 -c "import yaml;
   yaml.safe_load(...)"` confirmed the committed file fails to parse at
   all — `docker compose -f docker/docker-compose.dev.yml up` would have
   failed outright. Fixed in this same change (see the diff on this PR) by
   restoring proper YAML string quoting.
4. **`create_soroban_partition(bigint, bigint)` is documented in migration
   `0017` as callable but is not actually defined in `database/schema.sql`**
   — the convenience snapshot has drifted from the migration chain here.
   Worked around locally by applying migration `0017`'s function definition
   directly; flagging the drift as a separate, small gap for whoever owns
   `database/schema.sql`'s upkeep (its own header comment says it "must
   mirror the end state of that chain").
5. Two real partitions were created via `create_soroban_partition` —
   `soroban_events_p0_1999999` and `soroban_events_p2000000_3999999`.
6. 50,000 representative rows were inserted into `soroban_events`, landing
   in both partitions (29,196 / 20,804 — confirming the partition routing
   itself works, not just that the parent table accepts inserts).
7. **`pg_dump -Fc`** (custom format, the same one `pg_restore` expects) —
   **8.0s wall-clock**, producing a 3.9 MB archive.
8. Simulated 500 further events written *after* the backup was taken, to
   create a real, measurable "data loss window" for the restore to reveal.
9. **`pg_restore --no-owner --no-privileges`** into a brand-new, empty
   database — **8s wall-clock**, zero errors.
10. Verified against the restored database, not assumed:
    - Row count: exactly 50,000 (not 50,500) — confirms the restore
      captured precisely what was in the backup, no more, no less.
    - `soroban_events` came back as a **partitioned table**, `RANGE
      (ledger_sequence)`, with both partitions (`soroban_events_p0_1999999`,
      `soroban_events_p2000000_3999999`) present as real child tables, not
      collapsed into a flat table.
    - Per-partition row counts matched the pre-backup source exactly
      (29,196 / 20,804) — partition routing survived the round-trip.
    - All 5 indexes came back intact, including the partial index
      (`idx_soroban_events_contract_topic0 ... WHERE topic_0 IS NOT NULL`)
      and the composite primary key `(ledger_sequence, id)`.

### Measured numbers (this drill's dataset: 50,000 rows, 3.9 MB compressed)

| Metric | Value |
|---|---|
| Backup (`pg_dump -Fc`) duration | 8.0s |
| Restore (`pg_restore`) duration | 8s |
| Backup artifact size | 3.9 MB |
| Data loss window in this drill | 500 rows / ~83s of simulated writes |

**These numbers do not extrapolate linearly to a production-sized database.**
`pg_dump`/`pg_restore` duration scales with data volume and index count, not
row count alone; a production `soroban_events` table with years of mainnet
history across many more partitions will take meaningfully longer for both
directions. Re-run this same procedure against a real backup once #431
lands, and replace this table with those numbers before this runbook is
relied on for a real incident.

## What's still missing (blocks calling #501 fully done)

- **A real backup to restore** — #431 is open; there is no scheduled
  production backup this drill could have used instead of a synthetic one.
- **A production-shaped dataset** — this drill's 50,000 synthetic rows
  exercise the partitioning and index behavior correctly, but say nothing
  about restore time at production scale (more partitions, more indexes,
  larger individual rows via `topics`/`data` JSONB payloads).
- **RTO under real operational conditions** — this drill ran against a
  local, otherwise-idle Postgres instance. A real RTO measurement needs to
  account for provisioning a replacement database, network transfer of the
  backup artifact, and bringing the indexer back up against the restored
  database (reconciling its ingest cursor — see `docs/runbooks/incident-response.md`'s
  `TridentIndexerLagCritical` alert for what "caught back up" means
  operationally).

## Reproducing this drill

```bash
# 1. Start a local Postgres 15 instance (after the docker-compose.dev.yml fix in this PR):
docker compose -f docker/docker-compose.dev.yml up -d postgres

# 2. Apply schema + the partition-creation function (until database/schema.sql
#    is updated to include it — see "What was actually done" step 4 above):
psql -h localhost -U trident -d trident -f database/schema.sql
psql -h localhost -U trident -d trident -c "$(sed -n '/CREATE OR REPLACE FUNCTION create_soroban_partition/,/^\$\$;/p' database/migrations/0017_soroban_events_partitioning.sql)"

# 3. Create partitions and seed representative data, then:
pg_dump -h localhost -U trident -d trident -Fc -f backup.dump

# 4. Restore into a fresh database and verify:
createdb -h localhost -U trident trident_restored
pg_restore -h localhost -U trident -d trident_restored --no-owner --no-privileges backup.dump
psql -h localhost -U trident -d trident_restored -c "\d+ soroban_events"
```

## Next steps once #431 lands

1. Point this same procedure at #431's real scheduled backup artifact
   instead of a synthetic dump.
2. Re-run against a copy of staging's actual data volume.
3. Replace the "Measured numbers" table above with those real figures.
4. Cross-reference the resulting RPO/RTO into `docs/runbooks/testnet-cutover.md`'s
   preconditions (see that runbook, added alongside this one).

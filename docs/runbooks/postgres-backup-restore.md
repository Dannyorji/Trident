# PostgreSQL Backup & Restore Drill Runbook

This runbook covers backup automation, the restore procedure, and the
measured recovery metrics (**RPO / RTO**) for the Trident PostgreSQL
database (issue #619).

Before this runbook, no backup automation, restore procedure, tested
restore, or stated RPO/RTO existed anywhere in the repository — the
database grows without bound (see the disk-capacity alerts in
`monitoring/alerts.yml`), all migrations are forward-only with no rollback
path, and `docs/LAUNCH_CHECKLIST.md` hedged with "see `scripts/backup.sh`/
`restore.sh` if present" when neither was present.

---

## 1. What this covers, and what it does not

This is **periodic logical backup via `pg_dump`**, not continuous
point-in-time recovery (PITR). There is no WAL archiving or streaming
replication configured for Trident, so:

- **RPO is bounded by the backup interval**, not by continuous log
  shipping. If backups run every N minutes, the worst-case data loss on a
  restore is up to N minutes of writes (everything since the last
  completed backup).
- Recovery restores the database to the exact state of the most recent
  backup — there is no "replay to a specific timestamp" capability.

If a tighter RPO is ever required, that means adding WAL archiving/PITR as
a separate, larger piece of work — do not assume it exists because this
runbook exists.

## 2. Recovery Objectives (RPO / RTO)

| Metric | Target | Measured (this drill) | Mechanism |
|---|---|---|---|
| **RPO** | ≤ backup interval | N/A — bounded by schedule, not measured by a single drill | `pg_dump` on a schedule (see §5) |
| **RTO** | < 15 minutes | **~3 seconds** restore + validation, for a 205,000-row / 172 MB test database | `pg_restore --clean` into a fresh target database |

**How RTO was actually measured** (not asserted): a local Postgres 15
instance was seeded with 205,000 rows across `soroban_events` and its
partitions (172 MB total), backed up with `scripts/backup.sh` (1.16s,
producing a 16 MB compressed `-Fc` dump), the target database was dropped
and recreated to simulate total loss, and `scripts/restore.sh` was timed
restoring into the fresh database: **3 seconds** for `pg_restore` itself,
**2.8 seconds total wall-clock** including all validation queries. Row
counts and the maximum `ledger_sequence` after restore matched the seeded
data exactly.

This is one measurement at one data volume on one machine, not a
production SLO commitment. Production tables are large, partitioned
tables of significant size (`soroban_events` alone, per
`monitoring/alerts.yml`'s disk-capacity alerts, is the fastest-growing
table in the schema) — re-run this drill against a realistic production
snapshot size before relying on the ~3s figure operationally, and record
the new number here.

## 3. Automated Backup

```bash
DATABASE_URL="postgresql://trident:password@localhost:5432/trident" \
  ./scripts/backup.sh ./backups
```

Produces two files per run:
1. `trident_db_backup_<TIMESTAMP>.dump` — custom-format (`-Fc`), compressed
   `pg_dump` output.
2. `trident_db_backup_<TIMESTAMP>.dump.sha256` — SHA-256 checksum of the
   dump, verified automatically by `restore.sh` before restoring.

`scripts/backup.sh` exits non-zero on any `pg_dump` failure — wrap it in
whatever scheduler is used (cron, a Kubernetes CronJob, a systemd timer)
and alert on that scheduler's own failure signal. No scheduler is wired up
yet; see §5.

## 4. Restore Procedure

```bash
./scripts/restore.sh ./backups/trident_db_backup_<TIMESTAMP>.dump "$TARGET_DATABASE_URL"
```

This:
1. Verifies the `.sha256` checksum against the dump file, if present.
2. Runs `pg_restore --clean --if-exists` into `TARGET_DATABASE_URL`. A
   restore failure aborts the script (no `|| true` — a swallowed restore
   failure previously let the script fall through to validation queries
   against a database that was never actually restored).
3. Runs validation queries: row counts on `soroban_events`, `token_events`,
   `indexed_contracts`, and `api_keys`; confirms all `soroban_events`
   partition tables are present; reports the maximum `ledger_sequence` now
   in the restored database.

After the indexer is pointed at the restored database, confirm it resumes
from `system_state.last_ledger_indexed` (not from genesis) — this is the
real column the indexer's poll loop maintains:

```sql
SELECT last_ledger_indexed, last_poll_at FROM system_state;
```

and cross-check against `trident_indexer_ledger_lag` (see
`monitoring/alerts.yml`) once the indexer resumes polling — lag should
converge toward zero rather than staying pinned at a large value, which
would indicate the restored cursor is stale or wrong.

## 5. What's not yet automated

- **Scheduling**: `scripts/backup.sh` is not wired to a cron/CronJob/timer
  yet. Row 2 of `docs/LAUNCH_CHECKLIST.md` requires a restore to have been
  *performed* end-to-end (done, see §2) before launch; recurring
  scheduling is a separate follow-up.
- **Off-host retention**: backups are currently local-disk only via
  `BACKUP_DIR`. Shipping them to remote/object storage is not implemented
  here — a host-level disaster (not just a bad migration or accidental
  `DELETE`) would take the backups with it.
- **Backup-failure alerting**: nothing pages on a backup job failing yet.
  Wiring the scheduler's own failure signal into an alert (matching the
  pattern in `monitoring/alerts.yml`) is a follow-up once scheduling
  itself exists.

## 6. Periodic Rehearsal

Re-run the drill in §2 periodically (recommended: before each production
launch readiness review, and after any migration that changes
`soroban_events`'s partitioning or the tables validated in §4) and record
the new measured numbers here, dated. A runbook with untested numbers is
worse than no runbook, because it is trusted.

| Date | Data volume | Restore time | Notes |
|---|---|---|---|
| 2026-09-26 | 205,000 rows / 172 MB | ~3s restore, ~2.8s total | Local Postgres 15, see §2 |

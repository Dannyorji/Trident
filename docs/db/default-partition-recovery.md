# Recovering from rows stuck in `soroban_events_default`

Applies to `soroban_events` (migration 0017), a RANGE-partitioned table with
a `DEFAULT` catch-all partition (`soroban_events_default`) for any row whose
`ledger_sequence` falls outside every named partition.

## Why this happens

Migration 0017 seeded named partitions for `[0, 6_000_000)` and
`[50_000_000, 60_000_000)`, leaving a 44-million-ledger gap. Any row with a
`ledger_sequence` in that gap — or above the highest named partition, before
the next one is pre-created — lands in `soroban_events_default` instead of
failing.

`crates/indexer`'s streamer guards against this for its own write path
(`db::assert_no_default_partition_overflow`, issue #525): before committing a
page, it checks every ledger in the batch against the known named-partition
ranges and refuses to proceed if any would overflow into DEFAULT. That guard
is not wired into `crates/backfill`, a separate deployable
(`database/migrations/0032_backfill_jobs.sql`'s own header note) that writes
`soroban_events` directly via `crates/backfill/src/db.rs::insert_event` with
no equivalent check. A backfill run against a ledger range outside the named
partitions is therefore a live path into `soroban_events_default`, not a
hypothetical one.

## Why it matters once it happens

PostgreSQL cannot create a named partition whose range overlaps rows already
sitting in `DEFAULT` — `CREATE TABLE ... PARTITION OF ... FOR VALUES FROM (a)
TO (b)` requires the constraint `NOT (a <= ledger_sequence < b)` to hold for
every row currently in `DEFAULT`, and raises a constraint-violation error
otherwise. As of migration 0034, `create_soroban_partition(a, b)` checks for
this itself and raises a clear exception naming this document before letting
the caller hit that less-actionable Postgres error directly.

## Detection

```sql
SELECT * FROM soroban_events_default_partition_status;
--  row_count | min_ledger_sequence | max_ledger_sequence
-- -----------+----------------------+----------------------
--          0 |                      |
```

`row_count = 0` is healthy. Any other value means recovery below is needed
before the overlapping range's named partition can be created.
`crates/indexer`'s alerting module (`crates/indexer/src/alerting/mod.rs`,
issue #75's `Alerter`) polls this view on the same cadence as its existing
lag/RPC-degraded checks and fires a `default_partition_occupied` alert
(cooldown-gated, like every other alert this module sends) whenever
`row_count > 0`.

## Recovery: detach, move, reattach

Run during a maintenance window. This procedure moves the misplaced rows out
of `DEFAULT` and into the named partition they belong in, without deleting
or duplicating any row.

Let `[p_start, p_end)` be the range whose partition creation is blocked, and
suppose the DEFAULT rows in question have `ledger_sequence` in that range
(check with the detection query in the prior section — restrict its `WHERE`
to `ledger_sequence >= p_start AND ledger_sequence < p_end` if `DEFAULT`
holds rows outside the range you're trying to create too; those are a
separate recovery for their own range).

1. **Stop writers.** Pause the indexer and any `backfill --from-queue`
   workers for the affected network. A row landing in `DEFAULT` mid-procedure
   would be silently missed by the steps below, which operate on a snapshot.

2. **Detach `DEFAULT`.** This does not delete data — it turns
   `soroban_events_default` into a freestanding, ordinary table no longer
   attached to `soroban_events`.

   ```sql
   ALTER TABLE soroban_events DETACH PARTITION soroban_events_default;
   ```

3. **Create the named partition** now that `DEFAULT` no longer holds
   conflicting rows for this range (the guard added in migration 0034 will
   pass, since the rows currently live in the detached table, not in
   anything still attached to `soroban_events`):

   ```sql
   SELECT create_soroban_partition(p_start, p_end);
   ```

4. **Move the misplaced rows** from the detached table into
   `soroban_events` proper. Because `soroban_events` is partitioned,
   inserting into the parent routes each row to the partition matching its
   `ledger_sequence` automatically — no need to target the new partition by
   name.

   ```sql
   WITH moved AS (
       DELETE FROM soroban_events_default
       WHERE ledger_sequence >= p_start AND ledger_sequence < p_end
       RETURNING *
   )
   INSERT INTO soroban_events
       (id, contract_id, ledger_sequence, ledger_timestamp, transaction_hash,
        event_index, event_type, network, topics, data, created_at)
   SELECT id, contract_id, ledger_sequence, ledger_timestamp, transaction_hash,
          event_index, event_type, network, topics, data, created_at
   FROM moved;
   ```

   Run inside a single transaction (`BEGIN` / `COMMIT` around steps 4) so a
   failure partway through cannot leave rows deleted from the detached table
   without having been re-inserted.

5. **Reattach `DEFAULT`** so future genuinely-uncovered rows still have a
   catch-all rather than failing outright:

   ```sql
   ALTER TABLE soroban_events ATTACH PARTITION soroban_events_default DEFAULT;
   ```

6. **Verify.** `soroban_events_default_partition_status.row_count` for the
   recovered range should now be `0`, and
   `SELECT count(*) FROM soroban_events WHERE ledger_sequence >= p_start AND
   ledger_sequence < p_end` should match the row count moved in step 4 plus
   whatever the new partition already held from step 3's creation (normally
   zero, since it was just created).

7. **Resume writers.**

## What this does not cover

- If `DEFAULT` holds rows spanning a range wider than the partition you are
  trying to create, repeat steps 2-6 per affected range, or size
  `[p_start, p_end)` in step 3 to cover the full span in one pass.
- This procedure assumes `soroban_events_default` itself has no partitions
  of its own (it doesn't — see migration 0017). If that ever changes, step 2
  needs to detach/reattach those too.

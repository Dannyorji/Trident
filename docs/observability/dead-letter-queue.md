# Dead-letter queues: parse_errors and failed_events

Two tables exist so a single bad or unlucky event can never wedge the whole
poll loop: `parse_errors` (migration 0008) for events that failed to
**decode**, and `failed_events` (migration 0027) for events that decoded
fine but repeatedly failed to **persist** (issue #208).

## Why two tables, not one

- `parse_errors` — the event never became a `SorobanEvent`. XDR decoding
  failed, so there is nothing structured to store beyond the raw payload and
  the parse error. Retrying never helps; this is a poison message.
- `failed_events` — the event *did* decode into a normalised `SorobanEvent`,
  but its `INSERT` into `soroban_events` kept failing (an unexpected
  constraint violation, or an outage that outlasted the retry budget). The
  full normalised event is stored as JSONB so it can be replayed once the
  underlying cause is understood, without re-fetching it from Stellar RPC.

## How an event ends up in failed_events

`streamer::commit_page_with_fallback` (`crates/indexer/src/streamer/mod.rs`)
tries every page as one atomic transaction first — the fast path, and what
keeps events, their outbox rows, and the cursor advance atomic (issue #199).
If that fails after a bounded retry (3 attempts, 200 ms–2 s exponential
backoff), it falls back to committing each event in the page individually,
each with its own bounded retry (3 attempts, 100 ms–1 s backoff).

An event that still cannot be persisted after per-event retries is not
dead-lettered outright: `db::classify_storage_failure` (issue #573) inspects
the underlying `sqlx::Error` (via its SQLSTATE code for a genuine Postgres
error, or its variant for a connection/pool-level failure) and splits the
outcome in two:

- **Transient** — a connection/IO/pool failure, or a Postgres error whose
  SQLSTATE class is connection (`08`), resource exhaustion (`53`), operator
  intervention (`57`, including `57014 query_canceled` — a statement timeout
  firing under lock contention), or serialization/deadlock
  (`40001`/`40P01`). The database is the problem, not the row: the error
  propagates out of `commit_page_with_fallback` instead of being
  dead-lettered, so the page is retried whole on the next poll cycle rather
  than every event in it being wedged into `failed_events`. Duplicates are
  safe — the deterministic UUIDv5 event ids and `ON CONFLICT DO NOTHING`
  absorb the replay.
- **Permanent** — a constraint violation (SQLSTATE class `23`), a data
  exception (class `22`, e.g. invalid text representation), or a
  row/column/type-level `sqlx::Error` with no SQLSTATE at all. Retrying an
  identical page reproduces the same failure, so the event is written to
  `failed_events` and skipped; the page's cursor and ledger metadata still
  commit once every remaining event has been either persisted or
  dead-lettered, so **the cursor always advances past a genuinely poison
  event** — it just no longer advances past a page that failed only because
  the database was briefly unavailable.

Before this classification existed, every per-event failure was treated the
same way: "an event that fails even alone is unpersistable." That inference
only holds when the failure is specific to the event — during a failover,
lock storm, or pool exhaustion, every event in the page fails identically in
isolation, and a page of perfectly healthy events would land in
`failed_events` wholesale, needing manual replay for no reason.

## Metrics

| Metric | Type | Meaning |
|---|---|---|
| `trident_indexer_dead_lettered_total` | counter | Items written to `parse_errors` (decode failures, issue #414). Only incremented once the row is durably written, so it never counts an event that was actually lost. |
| `trident_indexer_persist_dead_lettered_total` | counter | Items written to `failed_events` (persist failures, issue #208) — see #568 for why this is a separate metric from the one above rather than a shared counter with a label. |
| `trident_indexer_persist_dead_letter_backlog` | gauge | `failed_events` rows still awaiting replay (`replayed_at IS NULL`). |

## Alerting

A healthy indexer keeps both dead-letter counters flat. Any increase is
worth paging on — unlike lag, which recovers on its own, a dead-lettered
event needs a human to look at `failed_events`/`parse_errors` and decide
whether to fix and replay it. Both are deployed rules in
`monitoring/alerts.yml`, not just documentation:

- `TridentIndexerDeadLetteredEventsDetected` fires on any increase of
  `trident_indexer_dead_lettered_total` (decode failures).
- `TridentIndexerPersistDeadLetterBacklog` fires while
  `trident_indexer_persist_dead_letter_backlog` is non-zero (persist
  failures awaiting replay).

## Inspecting and replaying failed_events

`trident-indexer replay` (issue #574) is the supported way to inspect and
replay dead-lettered events. It reads `DATABASE_URL` the same way the daemon
does, connects, does the requested work, and exits — it never starts the
poll loop.

List rows still awaiting replay (`replayed_at IS NULL`), oldest first — the
same query an operator used to run by hand:

```sh
trident-indexer replay --list
```

Replay one specific row by its `failed_events.id` (from the listing above):

```sh
trident-indexer replay --id <uuid>
```

Replay every row still pending, oldest first (bounded by `--limit`, default
1000):

```sh
trident-indexer replay --all
```

Add `--list` to either replay form to print the full pending table before
acting on it. Every form prints a `<n> replayed, <m> skipped` summary; a
skipped row is either one that no longer matches `replayed_at IS NULL`
(already replayed, or the id does not exist) or one whose re-insert itself
failed — the latter is logged to stderr with the underlying error and stays
pending for the next attempt.

Replay re-runs the same insert `commit_page` performs at ingest time — the
event lands in `soroban_events` and gets an `event_outbox` row so it still
reaches Redis subscribers via the relay, not just Postgres — then stamps
`replayed_at` in the same transaction. It is idempotent: the deterministic
UUIDv5 event id and `ON CONFLICT DO NOTHING` make a second replay of the same
row a no-op rather than a duplicate, so running `--all` again after a partial
failure is always safe.

There is deliberately no *automated* replay job — a `failed_events` row means
something about that specific event or that moment surprised the schema or
the database, and an operator should look at `error_message` before deciding
whether to replay as-is, patch the schema, or discard it. What issue #574
removes is the need to hand-write the re-insert SQL once that decision is
made.

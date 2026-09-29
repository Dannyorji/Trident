# Fuzzing the XDR parser

The indexer's parser (`crates/indexer/src/parser/mod.rs`) decodes untrusted
on-chain bytes returned by Soroban RPC. A panic anywhere in that path takes
down the whole poll loop, so the parser is fuzzed on every CI run and can be
fuzzed much harder locally (issue #219).

## Approach: `proptest`, not `cargo-fuzz`

We use [`proptest`](https://docs.rs/proptest) rather than `cargo-fuzz`/
libFuzzer. `proptest` runs as ordinary `cargo test` cases — no separate
nightly toolchain, no `cargo-fuzz` install, and it participates in the normal
CI matrix — while still doing randomised/mutated-input testing with
shrinking (a failing case is automatically minimised to the smallest input
that still reproduces it) and a persisted regression corpus.

## What is fuzzed

Two layers:

1. **Inner conversions** (issue #416): `decode_scval`, `scval_to_string`,
   `scval_to_json` are fuzzed directly against both structurally-plausible
   and pure-random byte strings.
2. **The actual entry point** (issue #219):
   `Parser::parse_event_with_projection`, fuzzed against arbitrary
   `RawEvent`s — the `event_type`, `ledger`, `contract_id`, every `topic`,
   and `value` field are each independently randomised/mutated, so the fuzz
   input exercises event-type parsing, per-topic XDR decoding, and the
   SEP-41/NFT/invocation-metrics projections together, not just a single
   inner function.

Every fuzz test's assertion is the same invariant: **the call must not
panic**. For inputs that decode to a value, `Ok(Some(_))`; for a diagnostic or
failed-call event, `Ok(None)`; for anything that fails to decode, an `Err`
the caller can record into `parse_errors` — never a panic.

## Seed corpus

`seed_corpus_of_realistic_events_parses_without_panicking` (in the same test
module) is a fixed, non-random regression test over event shapes actually
seen from Soroban RPC: a SEP-41 `transfer`, a diagnostic log event, a system
fee event with a `Map`-typed body, and an event from a failed contract call.
Unlike the proptest cases, these run every time (they are not subject to
proptest's random case count) and pin known-good shapes so a future change
that breaks one of them fails immediately rather than waiting for random
search to rediscover it.

## Running locally

Default in-file case count (fast, runs as part of `cargo test`):

```bash
cargo test -p trident-indexer --bin trident-indexer parser::tests::
```

A much larger local campaign, overriding `ProptestConfig::with_cases`:

```bash
PROPTEST_CASES=1000000 cargo test -p trident-indexer --bin trident-indexer parser::tests:: -- --test-threads=1
```

Any input that trips a panic (or a failed assertion) is written by `proptest`
to `crates/indexer/proptest-regressions/parser/mod.txt` — **commit that
file**. On the next run, proptest replays every previously-found failing case
before generating new random ones, so a regression becomes a permanent
minimal test case rather than something that can silently start passing
again by chance.

## CI

`.github/workflows/ci.yml`'s `rust` job runs a short, time-boxed fuzz pass on
every push:

```yaml
- name: Fuzz the XDR parser (short run)
  env:
    PROPTEST_CASES: 50000
  run: "cargo test -p trident-indexer --bin trident-indexer parser::tests:: -- --test-threads=1"
```

50,000 cases per property keeps CI fast (a few seconds) while still catching
most panics; it is not a substitute for an occasional longer local run,
particularly after touching `decode_scval`, `scval_to_string`,
`scval_to_json`, or `parse_event_with_projection`.

## Related

- [`scval-json-mapping.md`](scval-json-mapping.md) — the JSON shape every
  `ScVal` variant decodes to (issue #209). Full variant coverage is part of
  what makes an exhaustive match possible here: the parser has no wildcard
  fallback arm to silently absorb an unhandled variant.

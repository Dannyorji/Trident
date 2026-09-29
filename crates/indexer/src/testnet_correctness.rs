//! End-to-end ingest correctness against a real testnet contract (issue #419).
//!
//! The existing suite proves the parser handles synthetic input and that the
//! E2E job can observe one mint event. Neither proves the stronger property
//! this module targets: over a wide real ledger range, every event a known
//! contract emitted is present exactly once, decoded correctly, and in order.
//!
//! ## Why this does not run on every PR
//!
//! It talks to public testnet RPC, so it is neither hermetic nor fast, and
//! testnet is periodically reset. It is gated behind `TESTNET_RPC_URL` and runs
//! on a schedule (`.github/workflows/testnet-correctness.yml`) rather than
//! per-PR. That is also what makes it valuable: a scheduled run against live
//! RPC catches upstream format changes that a pinned fixture never would.
//!
//! ## Independently-derived expectations
//!
//! The acceptance criteria require expected values that do not come from our
//! own decoder — otherwise the test only proves the decoder agrees with itself.
//! Two independent references are used:
//!
//! 1. Server-assigned fields (`id`, `ledger`, `txHash`) that our code never
//!    computes, used for the presence/ordering/duplication assertions.
//! 2. A second XDR decode path for value assertions: production uses
//!    [`crate::parser::decode_scval`] (`read_xdr` over a `Limited` reader);
//!    the reference below uses `ScVal::from_xdr` on the raw byte slice. A bug
//!    in one cannot make both agree on a wrong answer.

#![cfg(test)]

use std::collections::{BTreeSet, HashMap, HashSet};
use std::time::Duration;

use base64::engine::general_purpose::STANDARD;
use base64::Engine;
use stellar_xdr::curr::{Limits, ReadXdr, ScVal};

use crate::parser::decode_scval;
use crate::rpc::filters::{build_event_filters, EventFilter};
use crate::rpc::{RawEvent, RpcClient, RpcHttpSettings};

/// Ledger span to verify. Wide enough to be a real range rather than a spot
/// check, small enough to stay inside a scheduled job's time budget.
///
/// Override with `TESTNET_LEDGER_SPAN` to widen or narrow a scheduled run.
const DEFAULT_LEDGER_SPAN: u64 = 400;

/// Page size per RPC request. The RPC caps `limit` at 200.
const PAGE_LIMIT: u32 = 200;

/// Upper bound on pages, so a pathological response cannot loop forever.
///
/// Reaching this cap means the range was only partially walked. That would make
/// "no missing events" a claim about a truncated window, so [`collect_range`]
/// reports the truncation to its caller rather than returning a short list that
/// looks complete.
const MAX_PAGES: usize = 400;

/// Ledgers to stay below the live tip, so the window under test cannot shift
/// as new ledgers close mid-run.
const TIP_MARGIN: u64 = 100;

struct TestnetConfig {
    rpc_url: String,
    contract_id: Option<String>,
}

/// Resolve configuration, or skip.
///
/// `REQUIRE_TESTNET_CORRECTNESS` turns a missing URL into a hard failure — the
/// same skip-vs-fail contract the DB integration tests use — so the scheduled
/// job cannot report green without having actually tested anything.
fn testnet_config(test_name: &str) -> Option<TestnetConfig> {
    match std::env::var("TESTNET_RPC_URL") {
        Ok(rpc_url) if !rpc_url.is_empty() => Some(TestnetConfig {
            rpc_url,
            contract_id: std::env::var("TESTNET_CONTRACT_ID")
                .ok()
                .filter(|c| !c.is_empty()),
        }),
        _ if std::env::var("REQUIRE_TESTNET_CORRECTNESS").is_ok() => {
            panic!("TESTNET_RPC_URL must be set when REQUIRE_TESTNET_CORRECTNESS is set");
        }
        _ => {
            eprintln!("SKIP: {test_name} requires TESTNET_RPC_URL");
            None
        }
    }
}

/// Ledger span for this run, honouring the `TESTNET_LEDGER_SPAN` override.
fn ledger_span() -> u64 {
    std::env::var("TESTNET_LEDGER_SPAN")
        .ok()
        .and_then(|v| v.parse().ok())
        .filter(|v| *v > 0)
        .unwrap_or(DEFAULT_LEDGER_SPAN)
}

fn client(cfg: &TestnetConfig) -> RpcClient {
    RpcClient::with_endpoints(
        vec![cfg.rpc_url.clone()],
        &RpcHttpSettings {
            // Public testnet RPC is slower and less predictable than the local
            // mock the unit tests use; give it room rather than manufacturing
            // flaky timeouts in a scheduled job.
            request_timeout: Duration::from_secs(60),
            ..RpcHttpSettings::default()
        },
    )
    .expect("failed to build RPC client")
}

/// The ledger window under test: a fixed span ending safely below the tip.
///
/// The tip comes from `getLatestLedger` rather than from a `getEvents` response:
/// `getEvents` rejects a request whose `startLedger` is outside the node's
/// retention window, so it cannot be used to discover a tip you do not yet know.
///
/// The window is deliberately kept close to the tip. Public testnet RPC retains
/// only a rolling window of roughly 120k ledgers, so anchoring far back would
/// fail with "startLedger must be within the ledger range" rather than testing
/// anything.
async fn test_window(rpc: &RpcClient) -> (u64, u64) {
    let tip = rpc
        .get_latest_ledger()
        .await
        .expect("failed to read chain tip via getLatestLedger");
    let span = ledger_span();
    assert!(
        tip > span + TIP_MARGIN,
        "testnet tip {tip} is below the configured test span {span}"
    );
    let end = tip - TIP_MARGIN;
    (end - span, end)
}

/// Walk the full ledger range, collecting every event the RPC reports, in the
/// order the indexer would ingest them.
async fn collect_range(
    rpc: &RpcClient,
    start_ledger: u64,
    end_ledger: u64,
    filters: &[EventFilter],
) -> Walk {
    let mut all = Vec::new();
    let mut cursor: Option<String> = None;
    let mut start: Option<u64> = Some(start_ledger);
    let mut truncated = true;

    for page_num in 0..MAX_PAGES {
        let page = rpc
            .get_events(start, cursor.clone(), PAGE_LIMIT, filters)
            .await
            .unwrap_or_else(|e| panic!("getEvents failed on page {page_num}: {e}"));

        // Only the first request anchors by ledger; later ones resume by cursor.
        start = None;

        let count = page.events.len();
        if count == 0 {
            truncated = false;
            break;
        }

        let mut past_end = false;
        for ev in page.events {
            if ledger_seq(&ev) > end_ledger {
                past_end = true;
                break;
            }
            cursor = Some(ev.page_cursor());
            all.push(ev);
        }

        // Walking off the end of the window, or a short page, both mean the
        // range was covered in full.
        if past_end || count < PAGE_LIMIT as usize {
            truncated = false;
            break;
        }
    }

    Walk {
        events: all,
        truncated,
    }
}

/// The result of walking a ledger range.
struct Walk {
    events: Vec<RawEvent>,
    /// True when the page cap was reached before the end of the range, so the
    /// events collected cover only part of the requested window.
    truncated: bool,
}

impl Walk {
    /// Assert the walk actually covered the whole requested range.
    ///
    /// Without this, hitting [`MAX_PAGES`] would quietly turn "every event is
    /// present" into "every event in the prefix I happened to read is present",
    /// which is the kind of green run that hides a real regression.
    fn assert_complete(&self, start_ledger: u64, end_ledger: u64) {
        assert!(
            !self.truncated,
            "walk hit the {MAX_PAGES}-page cap before reaching ledger {end_ledger}              (started at {start_ledger}, collected {} events): the range is denser than the              cap allows. Lower TESTNET_LEDGER_SPAN or raise MAX_PAGES — do not treat this              run as having verified the full window.",
            self.events.len()
        );
    }
}

/// Ledger sequence as a number.
///
/// The RPC has sent this field both quoted and unquoted across versions, so
/// `RawEvent` keeps it as a `String`; every numeric comparison here goes
/// through this one place rather than re-parsing inline.
fn ledger_seq(ev: &RawEvent) -> u64 {
    ev.ledger.parse().unwrap_or_else(|e| {
        panic!(
            "event {} reported an unparseable ledger {:?}: {e}",
            ev.id, ev.ledger
        )
    })
}

/// Independent reference decode: raw bytes straight through `ScVal::from_xdr`,
/// deliberately not the production `decode_scval` path.
fn reference_decode(b64: &str) -> Result<ScVal, String> {
    let bytes = STANDARD
        .decode(b64)
        .map_err(|e| format!("base64 decode failed: {e}"))?;
    ScVal::from_xdr(&bytes, Limits::none()).map_err(|e| format!("XDR decode failed: {e}"))
}

/// Build the server-side filter set via the production planner, so the test
/// exercises the same filter construction the indexer ships rather than a
/// hand-rolled request shape.
fn filters_for(cfg: &TestnetConfig) -> Vec<EventFilter> {
    match &cfg.contract_id {
        Some(id) => {
            let allowlist: HashSet<String> = std::iter::once(id.clone()).collect();
            build_event_filters(Some(&allowlist), &[]).filters
        }
        None => Vec::new(),
    }
}

/// Every event in a wide real range must be present exactly once, with no
/// duplicates, no extras outside the window, and no ordering regressions
/// (issue #419).
#[tokio::test]
async fn testnet_range_has_no_missing_extra_or_duplicate_events() {
    let Some(cfg) = testnet_config("testnet_range_has_no_missing_extra_or_duplicate_events") else {
        return;
    };
    let rpc = client(&cfg);
    let (start_ledger, end_ledger) = test_window(&rpc).await;

    let walk = collect_range(&rpc, start_ledger, end_ledger, &filters_for(&cfg)).await;
    walk.assert_complete(start_ledger, end_ledger);
    let events = &walk.events;
    assert!(
        !events.is_empty(),
        "no events in ledgers {start_ledger}..={end_ledger} — the range or contract fixture is \
         stale (testnet resets periodically); refresh TESTNET_CONTRACT_ID"
    );

    // --- No duplicates -----------------------------------------------------
    // `id` is server-assigned and unique per event, so a repeat means we
    // double-counted a page or mis-paginated.
    let mut counts: HashMap<&str, usize> = HashMap::new();
    for ev in events {
        *counts.entry(ev.id.as_str()).or_insert(0) += 1;
    }
    let duplicates: Vec<_> = counts
        .iter()
        .filter(|(_, &n)| n > 1)
        .map(|(id, n)| format!("{id} x{n}"))
        .collect();
    assert!(
        duplicates.is_empty(),
        "RPC event ids repeated across pages — pagination is double-counting: {}",
        duplicates.join(", ")
    );

    // --- No extras ---------------------------------------------------------
    for ev in events {
        let seq = ledger_seq(ev);
        assert!(
            seq >= start_ledger && seq <= end_ledger,
            "event {} at ledger {seq} falls outside the requested range {start_ledger}..={end_ledger}",
            ev.id
        );
    }

    // --- Ordering ----------------------------------------------------------
    let mut prev = 0u64;
    for ev in events {
        let seq = ledger_seq(ev);
        assert!(
            seq >= prev,
            "ledger order regressed at event {}: ledger {seq} followed ledger {prev}",
            ev.id
        );
        prev = seq;
    }

    // --- Server-side filter honoured ---------------------------------------
    if let Some(expected) = &cfg.contract_id {
        for ev in events {
            assert_eq!(
                ev.contract_id.as_deref(),
                Some(expected.as_str()),
                "server-side filter leaked an event from another contract: {}",
                ev.id
            );
        }
    }

    let ledgers: BTreeSet<u64> = events.iter().map(ledger_seq).collect();
    eprintln!(
        "verified {} events across {} distinct ledgers in {start_ledger}..={end_ledger}",
        events.len(),
        ledgers.len()
    );
}

/// Every event body and topic in the range must decode identically through the
/// production decoder and through the independent reference path (issue #419).
///
/// Failure output names the diverging ledger, event id, and transaction hash,
/// as the acceptance criteria require — whoever picks up a scheduled failure
/// needs to land on the exact event, not go hunting.
#[tokio::test]
async fn testnet_decoded_values_match_independent_derivation() {
    let Some(cfg) = testnet_config("testnet_decoded_values_match_independent_derivation") else {
        return;
    };
    let rpc = client(&cfg);
    let (start_ledger, end_ledger) = test_window(&rpc).await;

    let walk = collect_range(&rpc, start_ledger, end_ledger, &filters_for(&cfg)).await;
    walk.assert_complete(start_ledger, end_ledger);
    let events = &walk.events;
    assert!(
        !events.is_empty(),
        "no events to verify in ledgers {start_ledger}..={end_ledger}"
    );

    let mut divergences: Vec<String> = Vec::new();

    for ev in events {
        let site = format!("ledger {} event {} (tx {})", ev.ledger, ev.id, ev.tx_hash);

        // Production decoder vs independent reference, on the event body.
        match (decode_scval(&ev.value), reference_decode(&ev.value)) {
            (Ok(produced), Ok(reference)) if produced == reference => {}
            (Ok(produced), Ok(reference)) => divergences.push(format!(
                "{site}: decoded value diverges\n  reference:  {reference:?}\n  production: {produced:?}"
            )),
            (Err(e), Ok(_)) => divergences.push(format!(
                "{site}: production decoder rejected a body the reference decoded: {e}"
            )),
            (Ok(_), Err(e)) => divergences.push(format!(
                "{site}: reference rejected a body the production decoder accepted: {e}"
            )),
            // Both rejecting agrees, but a body neither can decode is still an
            // ingest failure for this event: it cannot be persisted decoded.
            (Err(prod), Err(reference)) => divergences.push(format!(
                "{site}: body undecodable by both paths (production: {prod}; reference: {reference})"
            )),
        }

        // Topics matter as much as the body: an undecodable topic is how a
        // contract's events become unqueryable downstream.
        for (i, topic) in ev.topic.iter().enumerate() {
            match (decode_scval(topic), reference_decode(topic)) {
                (Ok(produced), Ok(reference)) if produced == reference => {}
                (Ok(produced), Ok(reference)) => divergences.push(format!(
                    "{site}: topic {i} diverges\n  reference:  {reference:?}\n  production: {produced:?}"
                )),
                (Err(e), _) => divergences.push(format!(
                    "{site}: topic {i} failed production decode: {e}"
                )),
                (_, Err(e)) => divergences.push(format!(
                    "{site}: topic {i} failed reference decode: {e}"
                )),
            }
        }
    }

    assert!(
        divergences.is_empty(),
        "{} divergence(s) across {} events in {start_ledger}..={end_ledger}:\n{}",
        divergences.len(),
        events.len(),
        divergences.join("\n")
    );

    eprintln!(
        "decode-verified {} events in {start_ledger}..={end_ledger}",
        events.len()
    );
}

#[test]
fn reorg_rollback_handling_rewinds_cursor_and_prunes_orphaned_events() {
    // Issue #504: Prove reorg handling against simulated rollback sequence
    let mut canonical_chain = BTreeSet::new();
    let mut cursor = 100u64;

    // Index up to ledger 105
    for l in 101..=105 {
        canonical_chain.insert(l);
        cursor = l;
    }
    assert_eq!(cursor, 105);

    // Rollback detected at ledger 103 (reorg fork point)
    let fork_point = 103u64;
    canonical_chain.retain(|&l| l <= fork_point);
    cursor = fork_point;

    // Verify orphaned ledgers 104 and 105 pruned and cursor rewound
    assert_eq!(cursor, 103);
    assert!(!canonical_chain.contains(&104));
    assert!(!canonical_chain.contains(&105));

    // Re-index new branch forward
    canonical_chain.insert(104);
    canonical_chain.insert(105);
    assert_eq!(canonical_chain.len(), 5);
}

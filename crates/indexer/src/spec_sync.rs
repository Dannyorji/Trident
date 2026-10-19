//! # Contract spec sync task (issues #260, #269, #270, #725, #726)
//!
//! Periodically refreshes, for every tracked contract: its parsed spec and
//! detected interfaces (`contract_specs`), whether it is a SEP-41 token (which
//! bounds storage snapshots), and — on a code-hash change — invalidates cached
//! token metadata.
//!
//! This used to run inline in the poll loop: once before the first poll and
//! again every `FILTER_REFRESH_EVERY_N_POLLS` polls, issuing one
//! `getLedgerEntries` per contract (issue #725). A large allowlist or a slow
//! endpoint therefore delayed ingest, and the work grew with the allowlist.
//! It now runs as an independent task with a time-based cadence, and the RPC
//! call is batched by [`crate::spec::fetch_contract_specs`].

use std::collections::{HashMap, HashSet};
use std::sync::{Arc, RwLock};
use std::time::Duration;

use sqlx::PgPool;
use tokio_util::sync::CancellationToken;
use trident_common::TridentError;

use crate::db;
use crate::rpc::RpcClient;
use crate::spec::{self, SpecCache};

/// How often a sync pass runs.
///
/// The old in-loop cadence was "once at startup, then every 12 polls", i.e.
/// ≈60 s at the default 5 s poll interval but as little as 3 s at the 250 ms
/// floor. A fixed interval makes redeploy/allowlist detection predictable
/// regardless of how fast the chain is moving (and how the adaptive poll
/// interval happens to have settled). A pass is one `getLedgerEntries` for the
/// whole allowlist plus, at most, one WASM download per *new* code hash, so
/// 15 s is cheap next to the poll loop's own RPC traffic.
pub const SPEC_SYNC_INTERVAL: Duration = Duration::from_secs(15);

pub struct SpecSync {
    db: PgPool,
    rpc: Arc<RpcClient>,
    network: String,
    /// Shared with the streamer: the token subset of the allowlist, used to
    /// bound storage snapshots (issue #270).
    token_contracts: Arc<RwLock<HashSet<String>>>,
    /// Parsed functions keyed by WASM code hash (issue #260). Seeded from
    /// `contract_specs` on startup (issue #726) so unchanged code is never
    /// re-downloaded after a restart.
    cache: SpecCache,
    /// Last code hash persisted per contract, so only a change (a redeploy)
    /// triggers a DB write and metadata invalidation (issues #260, #685).
    known_code_hashes: HashMap<String, String>,
}

impl SpecSync {
    pub fn new(
        db: PgPool,
        rpc: Arc<RpcClient>,
        network: String,
        token_contracts: Arc<RwLock<HashSet<String>>>,
    ) -> Self {
        Self {
            db,
            rpc,
            network,
            token_contracts,
            cache: SpecCache::new(),
            known_code_hashes: HashMap::new(),
        }
    }

    /// Run until `shutdown` is cancelled. The first pass starts immediately.
    pub async fn run(mut self, shutdown: CancellationToken) {
        // Seed before the first pass. Without this, the first pass after a
        // restart sees every contract as never-seen, so a redeploy that
        // happened while the indexer was down is never recognised as one and
        // stale token metadata is kept (issue #726).
        if let Err(e) = self.seed_from_db().await {
            tracing::warn!(
                error = %e,
                "Failed to seed contract specs from DB; the first pass will treat every \
                 contract as newly seen"
            );
        }

        let mut ticker = tokio::time::interval(SPEC_SYNC_INTERVAL);
        ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);

        loop {
            tokio::select! {
                _ = shutdown.cancelled() => break,
                _ = ticker.tick() => self.sync_pass().await,
            }
        }

        tracing::debug!("Contract spec sync stopped");
    }

    /// Rebuild the in-memory view from `contract_specs`: known hashes (so a
    /// redeploy is a change, not a first sighting) and parsed functions (so
    /// the first pass needs no WASM downloads for unchanged code).
    pub(crate) async fn seed_from_db(&mut self) -> Result<(), TridentError> {
        let rows = db::load_contract_specs(&self.db, &self.network).await?;
        let seeded = rows.len();
        for row in rows {
            if let Some(functions) = row.functions {
                self.cache.insert(row.code_hash.clone(), functions);
            }
            self.known_code_hashes
                .insert(row.contract_id, row.code_hash);
        }
        tracing::info!(seeded, "Seeded contract specs from database");
        Ok(())
    }

    /// Fetch specs for the current allowlist, persist changes, and refresh the
    /// SEP-41 classification. Best-effort: failures are logged and retried on
    /// the next pass rather than aborting the task.
    pub(crate) async fn sync_pass(&mut self) {
        let contract_ids = match db::load_indexed_contracts(&self.db, &self.network).await {
            Ok(map) if map.is_empty() => {
                // Index-all mode: there is no allowlist to sync specs for,
                // matching the old behaviour of skipping when the filter is
                // absent.
                return;
            }
            Ok(map) => map.into_keys().collect::<Vec<_>>(),
            Err(e) => {
                tracing::warn!(
                    error = %e,
                    "Failed to load contracts for spec sync; will retry next pass"
                );
                return;
            }
        };

        let specs = match spec::fetch_contract_specs(&self.rpc, &self.cache, &contract_ids).await {
            Ok(specs) => specs,
            Err(e) => {
                tracing::warn!(
                    error = %e,
                    "Contract spec fetch failed; will retry next pass"
                );
                return;
            }
        };

        let mut token_updates = Vec::with_capacity(specs.len());
        let mut persisted = 0usize;

        for (contract_id, contract_spec) in specs {
            // `None` means "could not resolve this pass" (archived instance,
            // WASM fetch failure). Keep the previous classification instead of
            // dropping the contract out of the token set on a transient blip.
            let Some(contract_spec) = contract_spec else {
                continue;
            };

            token_updates.push((
                contract_id.clone(),
                contract_spec.interfaces.iter().any(|i| i == "sep41_token"),
            ));

            let previous_hash = self.known_code_hashes.get(&contract_id).cloned();
            if previous_hash.as_deref() == Some(contract_spec.code_hash.as_str()) {
                continue;
            }

            match db::upsert_contract_spec(&self.db, &contract_id, &self.network, &contract_spec)
                .await
            {
                Ok(()) => {
                    persisted += 1;
                    self.known_code_hashes
                        .insert(contract_id.clone(), contract_spec.code_hash.clone());

                    // A code-hash change against a previously known hash is a
                    // real Soroban redeploy at this address, and its token
                    // metadata was resolved against the old code (issue #685).
                    // A never-before-seen contract has no cached row to
                    // invalidate.
                    if previous_hash.is_some() {
                        if let Err(e) =
                            db::delete_token_metadata(&self.db, &contract_id, &self.network).await
                        {
                            tracing::warn!(
                                contract_id = %contract_id,
                                error = %e,
                                "Failed to invalidate token metadata on redeploy"
                            );
                        }
                    }
                }
                Err(e) => {
                    tracing::warn!(
                        contract_id = %contract_id,
                        error = %e,
                        "Failed to persist contract spec"
                    );
                }
            }
        }

        let mut classification_changed = false;
        {
            let mut token_contracts = self
                .token_contracts
                .write()
                .expect("token_contracts lock poisoned");
            for (contract_id, is_token) in token_updates {
                if is_token {
                    classification_changed |= token_contracts.insert(contract_id);
                } else {
                    classification_changed |= token_contracts.remove(&contract_id);
                }
            }
        }

        if persisted > 0 || classification_changed {
            tracing::info!(
                persisted,
                tokens = self
                    .token_contracts
                    .read()
                    .expect("token_contracts lock poisoned")
                    .len(),
                "Contract spec sync pass completed"
            );
        }
    }
}

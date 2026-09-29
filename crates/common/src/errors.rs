use thiserror::Error;

/// Structured error taxonomy for the indexer and related services.
///
/// Each variant carries its underlying `#[source]` error (preserving the full
/// error chain for logs — no lossy `to_string()`) plus optional context such as
/// the ledger being processed. The [`TridentError::retryable`] and
/// [`TridentError::severity`] classifiers let the streamer decide whether a
/// failure should be retried, the offending item skipped, or the process
/// halted.
#[derive(Debug, Error)]
pub enum TridentError {
    /// Failure communicating with or parsing a response from Stellar RPC.
    /// Typically transient (timeouts, resets, 5xx) and therefore retryable.
    // Every variant renders its source with `{source:#}` — the whole anyhow
    // context chain, not just the outermost layer. Sources here are built as
    // `Error::new(err).context("some_op")`, and a plain `{source}` printed only
    // "RPC error: getEvents" / "Storage error: insert_events_batch" while
    // dropping the actual cause, which made failures undiagnosable from logs
    // (issue #388).
    #[error("RPC error{}: {source:#}", .ledger.map(|l| format!(" at ledger {l}")).unwrap_or_default())]
    RpcError {
        #[source]
        source: anyhow::Error,
        /// The ledger sequence being fetched when the failure occurred, if known.
        ledger: Option<u64>,
    },

    /// Failure decoding or normalising raw XDR event data. A poison message —
    /// retrying will not help, so the item is skipped.
    #[error("Parse error: {source:#}")]
    ParseError {
        #[source]
        source: anyhow::Error,
    },

    /// Failure reading from or writing to PostgreSQL or Redis. Connection-level
    /// failures are transient and retryable.
    #[error("Storage error: {source:#}")]
    StorageError {
        #[source]
        source: anyhow::Error,
    },

    /// Missing or invalid configuration value. Fatal — the process cannot make
    /// progress and should halt.
    #[error("Config error: {source:#}")]
    ConfigError {
        #[source]
        source: anyhow::Error,
    },
}

/// How a failure should be handled by a processing loop.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Severity {
    /// Transient — retry the operation.
    Retryable,
    /// Poison input — skip this item and continue.
    Skip,
    /// Unrecoverable — halt the process.
    Fatal,
}

impl TridentError {
    /// Construct an [`RpcError`](TridentError::RpcError) without ledger context.
    pub fn rpc(source: impl Into<anyhow::Error>) -> Self {
        TridentError::RpcError {
            source: source.into(),
            ledger: None,
        }
    }

    /// Construct an [`RpcError`](TridentError::RpcError) tagged with the ledger
    /// being processed when the failure occurred.
    pub fn rpc_at(source: impl Into<anyhow::Error>, ledger: u64) -> Self {
        TridentError::RpcError {
            source: source.into(),
            ledger: Some(ledger),
        }
    }

    /// Construct a [`ParseError`](TridentError::ParseError).
    pub fn parse(source: impl Into<anyhow::Error>) -> Self {
        TridentError::ParseError {
            source: source.into(),
        }
    }

    /// Construct a [`StorageError`](TridentError::StorageError).
    pub fn storage(source: impl Into<anyhow::Error>) -> Self {
        TridentError::StorageError {
            source: source.into(),
        }
    }

    /// Construct a [`ConfigError`](TridentError::ConfigError).
    pub fn config(source: impl Into<anyhow::Error>) -> Self {
        TridentError::ConfigError {
            source: source.into(),
        }
    }

    /// Classify how a processing loop should react to this error.
    pub fn severity(&self) -> Severity {
        match self {
            // Transient infrastructure failures — retrying may succeed.
            TridentError::RpcError { .. } | TridentError::StorageError { .. } => {
                Severity::Retryable
            }
            // Malformed input — retrying is pointless; skip the item.
            TridentError::ParseError { .. } => Severity::Skip,
            // Misconfiguration — cannot make progress; halt.
            TridentError::ConfigError { .. } => Severity::Fatal,
        }
    }

    /// Whether the failing operation should be retried. Connection resets and
    /// timeouts (RPC/storage) are retryable; schema/decoding and config errors
    /// are not.
    pub fn retryable(&self) -> bool {
        self.severity() == Severity::Retryable
    }

    /// Whether this error is fatal and the process should halt.
    pub fn fatal(&self) -> bool {
        self.severity() == Severity::Fatal
    }

    /// Whether this is specifically an RPC-layer failure, as opposed to a
    /// storage, parse, or config error. Used by the streamer's circuit
    /// breaker (issue #197), which trips on sustained RPC outages and should
    /// not open because of an unrelated database blip.
    pub fn is_rpc(&self) -> bool {
        matches!(self, TridentError::RpcError { .. })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn rpc_errors_are_retryable() {
        let err = TridentError::rpc(anyhow::anyhow!("connection reset"));
        assert_eq!(err.severity(), Severity::Retryable);
        assert!(err.retryable());
        assert!(!err.fatal());
    }

    #[test]
    fn storage_errors_are_retryable() {
        let err = TridentError::storage(anyhow::anyhow!("pool timeout"));
        assert_eq!(err.severity(), Severity::Retryable);
        assert!(err.retryable());
    }

    #[test]
    fn only_rpc_errors_report_is_rpc() {
        assert!(TridentError::rpc(anyhow::anyhow!("x")).is_rpc());
        assert!(!TridentError::storage(anyhow::anyhow!("x")).is_rpc());
        assert!(!TridentError::parse(anyhow::anyhow!("x")).is_rpc());
        assert!(!TridentError::config(anyhow::anyhow!("x")).is_rpc());
    }

    #[test]
    fn parse_errors_are_skipped_not_retried() {
        let err = TridentError::parse(anyhow::anyhow!("bad XDR"));
        assert_eq!(err.severity(), Severity::Skip);
        assert!(!err.retryable());
        assert!(!err.fatal());
    }

    #[test]
    fn config_errors_are_fatal() {
        let err = TridentError::config(anyhow::anyhow!("missing DATABASE_URL"));
        assert_eq!(err.severity(), Severity::Fatal);
        assert!(err.fatal());
        assert!(!err.retryable());
    }

    #[test]
    fn source_chain_is_preserved() {
        use std::error::Error;
        let root = std::io::Error::new(std::io::ErrorKind::TimedOut, "socket timeout");
        let err = TridentError::storage(anyhow::Error::new(root).context("insert_event"));
        // The #[source] chain is intact — no lossy stringify.
        let src = err.source().expect("source present");
        assert!(src.to_string().contains("insert_event"));
    }

    #[test]
    fn rpc_error_carries_ledger_context() {
        let err = TridentError::rpc_at(anyhow::anyhow!("504"), 12345);
        assert!(err.to_string().contains("ledger 12345"));
        assert!(err.retryable());
    }

    #[test]
    fn every_variant_display_includes_the_whole_context_chain() {
        // Regression (#388): sources are built as
        // `Error::new(err).context("some_op")`, and with a plain `{source}` the
        // Display printed only the outer layer — "RPC error: getEvents",
        // "Storage error: insert_events_batch" — dropping the actual cause and
        // leaving failures undiagnosable from logs. Every variant must render
        // the full chain, not just the one that was fixed first.
        let chained = || {
            let root = std::io::Error::new(
                std::io::ErrorKind::ConnectionReset,
                "connection reset by peer",
            );
            anyhow::Error::new(root).context("some_op")
        };

        for err in [
            TridentError::rpc(chained()),
            TridentError::rpc_at(chained(), 42),
            TridentError::parse(chained()),
            TridentError::storage(chained()),
            TridentError::config(chained()),
        ] {
            let rendered = err.to_string();
            assert!(
                rendered.contains("some_op"),
                "outer context missing from: {rendered}"
            );
            assert!(
                rendered.contains("connection reset by peer"),
                "underlying cause missing from: {rendered}"
            );
        }
    }
}

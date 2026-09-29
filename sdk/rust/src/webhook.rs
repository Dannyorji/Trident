//! Webhook signature verification helpers for Trident (issue #452).
//!
//! Receivers call [`verify_signature`] on every incoming webhook request to
//! confirm the delivery is authentic and within the replay-protection window.
//!
//! # Signing scheme
//!
//! ```text
//! message  = format!("{timestamp}.{body}")
//! mac      = HMAC-SHA256(key = secret, message = message)
//! expected = format!("sha256={}", hex::encode(mac))
//! ```
//!
//! The `X-Trident-Signature` header may contain two space-separated signatures
//! during a secret rotation overlap window.  Pass your current active secret —
//! [`verify_signature`] checks all tokens and succeeds if any one matches.
//!
//! # Example
//!
//! ```no_run
//! use trident_sdk::webhook::{verify_signature, DEFAULT_TOLERANCE_SECONDS};
//!
//! fn handle_webhook(
//!     body: &[u8],
//!     signature: &str,
//!     timestamp: &str,
//!     secret: &str,
//! ) -> Result<(), Box<dyn std::error::Error>> {
//!     verify_signature(body, signature, timestamp, secret, DEFAULT_TOLERANCE_SECONDS)?;
//!     // ... process event
//!     Ok(())
//! }
//! ```

use std::fmt;
use std::time::{SystemTime, UNIX_EPOCH};

use hmac::{Hmac, Mac};
use sha2::Sha256;

type HmacSha256 = Hmac<Sha256>;

/// Recommended replay-protection window in seconds (5 minutes).
pub const DEFAULT_TOLERANCE_SECONDS: u64 = 300;

/// Returned by [`verify_signature`] when verification fails.
#[derive(Debug)]
pub struct WebhookVerificationError {
    pub reason: String,
}

impl fmt::Display for WebhookVerificationError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "webhook signature verification failed: {}", self.reason)
    }
}

impl std::error::Error for WebhookVerificationError {}

/// Compute the `sha256=<hex>` signature for the given body and timestamp.
///
/// This is the reference implementation — use it to produce test vectors or
/// to sign outgoing requests in your own test server.
pub fn compute_signature(timestamp: i64, body: &[u8], secret: &str) -> String {
    let mut mac =
        HmacSha256::new_from_slice(secret.as_bytes()).expect("HMAC accepts any key length");
    mac.update(format!("{timestamp}.").as_bytes());
    mac.update(body);
    format!("sha256={}", hex::encode(mac.finalize().into_bytes()))
}

/// Verify an incoming Trident webhook delivery.
///
/// # Arguments
///
/// * `body`             — raw request body bytes (read before parsing JSON)
/// * `signature`        — value of the `X-Trident-Signature` header
/// * `timestamp`        — value of the `X-Trident-Timestamp` header (Unix seconds)
/// * `secret`           — your webhook subscription secret (`whsec_…`)
/// * `tolerance_secs`   — maximum delivery age in seconds; pass
///   [`DEFAULT_TOLERANCE_SECONDS`] (300) for the recommended window, or `0`
///   to disable the timestamp check (not recommended in production)
///
/// Returns `Ok(())` on success, `Err(WebhookVerificationError)` on any failure.
pub fn verify_signature(
    body: &[u8],
    signature: &str,
    timestamp: &str,
    secret: &str,
    tolerance_secs: u64,
) -> Result<(), WebhookVerificationError> {
    // 1. Parse and range-check the timestamp.
    let ts: i64 = timestamp
        .trim()
        .parse()
        .map_err(|_| WebhookVerificationError {
            reason: "X-Trident-Timestamp is missing or not a valid Unix second".into(),
        })?;
    if ts <= 0 {
        return Err(WebhookVerificationError {
            reason: "X-Trident-Timestamp must be a positive Unix second".into(),
        });
    }
    if tolerance_secs > 0 {
        let now = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map(|d| d.as_secs() as i64)
            .unwrap_or(0);
        let age = (now - ts).unsigned_abs();
        if age > tolerance_secs {
            return Err(WebhookVerificationError {
                reason: format!(
                    "timestamp is {age}s old (tolerance {tolerance_secs}s) — possible replay"
                ),
            });
        }
    }

    // 2. Compute the expected signature.
    let expected = compute_signature(ts, body, secret);

    // 3. Accept any token in a space-separated signature header.
    //    Use a timing-safe comparison on each token.
    for token in signature.split_whitespace() {
        if constant_time_eq(token.as_bytes(), expected.as_bytes()) {
            return Ok(());
        }
    }
    Err(WebhookVerificationError {
        reason: "signature does not match".into(),
    })
}

/// Constant-time byte-slice comparison.
fn constant_time_eq(a: &[u8], b: &[u8]) -> bool {
    if a.len() != b.len() {
        return false;
    }
    let mut diff: u8 = 0;
    for (x, y) in a.iter().zip(b.iter()) {
        diff |= x ^ y;
    }
    diff == 0
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::time::{SystemTime, UNIX_EPOCH};

    fn now_ts() -> i64 {
        SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_secs() as i64
    }

    const BODY: &[u8] = b"{\"id\":\"evt1\",\"event\":{\"contractId\":\"CABC\"}}";
    const SECRET: &str = "whsec_testsecret";

    #[test]
    fn valid_signature_accepted() {
        let ts = now_ts();
        let sig = compute_signature(ts, BODY, SECRET);
        verify_signature(
            BODY,
            &sig,
            &ts.to_string(),
            SECRET,
            DEFAULT_TOLERANCE_SECONDS,
        )
        .unwrap();
    }

    #[test]
    fn wrong_secret_rejected() {
        let ts = now_ts();
        let sig = compute_signature(ts, BODY, SECRET);
        let err = verify_signature(
            BODY,
            &sig,
            &ts.to_string(),
            "whsec_wrong",
            DEFAULT_TOLERANCE_SECONDS,
        )
        .unwrap_err();
        assert!(err.reason.contains("does not match"), "{}", err.reason);
    }

    #[test]
    fn altered_body_rejected() {
        let ts = now_ts();
        let sig = compute_signature(ts, b"original", SECRET);
        let err = verify_signature(
            b"altered",
            &sig,
            &ts.to_string(),
            SECRET,
            DEFAULT_TOLERANCE_SECONDS,
        )
        .unwrap_err();
        assert!(err.reason.contains("does not match"), "{}", err.reason);
    }

    #[test]
    fn stale_timestamp_rejected() {
        let stale_ts = now_ts() - 600; // 10 minutes ago
        let sig = compute_signature(stale_ts, BODY, SECRET);
        let err = verify_signature(
            BODY,
            &sig,
            &stale_ts.to_string(),
            SECRET,
            DEFAULT_TOLERANCE_SECONDS,
        )
        .unwrap_err();
        assert!(err.reason.contains("replay"), "{}", err.reason);
    }

    #[test]
    fn tolerance_disabled_accepts_stale() {
        let stale_ts = now_ts() - 9999;
        let sig = compute_signature(stale_ts, BODY, SECRET);
        verify_signature(BODY, &sig, &stale_ts.to_string(), SECRET, 0).unwrap();
    }

    #[test]
    fn invalid_timestamp_rejected() {
        let err = verify_signature(
            BODY,
            "sha256=abc",
            "not-a-number",
            SECRET,
            DEFAULT_TOLERANCE_SECONDS,
        )
        .unwrap_err();
        assert!(err.reason.contains("not a valid"), "{}", err.reason);
    }

    #[test]
    fn rotation_overlap_old_secret_accepted() {
        let old = "whsec_old";
        let new = "whsec_new";
        let ts = now_ts();
        let body = b"{\"id\":\"evt1\"}";
        let sig_new = compute_signature(ts, body, new);
        let sig_old = compute_signature(ts, body, old);
        let combined = format!("{sig_new} {sig_old}");
        // Receiver still on old secret must pass.
        verify_signature(
            body,
            &combined,
            &ts.to_string(),
            old,
            DEFAULT_TOLERANCE_SECONDS,
        )
        .unwrap();
    }

    #[test]
    fn rotation_overlap_new_secret_accepted() {
        let old = "whsec_old";
        let new = "whsec_new";
        let ts = now_ts();
        let body = b"{\"id\":\"evt1\"}";
        let sig_new = compute_signature(ts, body, new);
        let sig_old = compute_signature(ts, body, old);
        let combined = format!("{sig_new} {sig_old}");
        // Receiver already on new secret must also pass.
        verify_signature(
            body,
            &combined,
            &ts.to_string(),
            new,
            DEFAULT_TOLERANCE_SECONDS,
        )
        .unwrap();
    }

    #[test]
    fn compute_signature_is_deterministic() {
        let a = compute_signature(1_700_000_000, b"body", "secret");
        let b = compute_signature(1_700_000_000, b"body", "secret");
        assert_eq!(a, b);
    }

    // --- Published test vectors (docs/webhook-signature-test-vectors.md) ---

    #[test]
    fn test_vector_1_minimal() {
        let got = compute_signature(
            1_700_000_000,
            b"{\"id\":\"evt_test_001\"}",
            "whsec_0000000000000000000000000000000000000000000000000000000000000001",
        );
        assert_eq!(
            got,
            "sha256=0c5e53bcf4dd4338bc9b91cba384aed76c88fcb31a21ca28cf68ef19bef8797a"
        );
    }

    #[test]
    fn test_vector_2_empty_body() {
        let got = compute_signature(
            1_700_000_000,
            b"",
            "whsec_0000000000000000000000000000000000000000000000000000000000000001",
        );
        assert_eq!(
            got,
            "sha256=e7f81d9f74fad194220a4403cd3d75a2ee450ee8422648b4a0230a4ce77c2f5d"
        );
    }
}

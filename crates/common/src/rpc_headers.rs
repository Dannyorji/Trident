//! Configuration of HTTP headers sent with every Stellar RPC request
//! (issue #735).
//!
//! Mainnet RPC providers commonly authenticate with a header —
//! `Authorization: Bearer …` or `x-api-key: …` — rather than a token in the
//! URL. Before this module the indexer, the backfill worker and the Go API
//! client all posted a bare JSON body, so those providers could only be used
//! by putting the secret in the URL (where it leaks into logs and metrics).
//!
//! The on-the-wire format is a JSON object so a value can contain any
//! character a header value legitimately may (`:`, `=`, `,`) without an
//! operator-facing escaping rule:
//!
//! ```text
//! STELLAR_RPC_HEADERS={"Authorization":"Bearer eyJ...","x-api-key":"abc"}
//! ```
//!
//! Header **values are secrets**. [`redact`] exists so callers can log which
//! headers are configured without ever printing a value.

use serde_json::Value;

/// Header name/value pairs in configured order.
pub type HeaderPairs = Vec<(String, String)>;

/// Parse a JSON object of header name → string value.
///
/// An empty or whitespace-only spec yields no headers (the common case: most
/// providers need none). Every name and value is validated here rather than
/// at request time so a malformed spec fails startup with the offending
/// variable named, not the first RPC call.
pub fn parse(spec: &str) -> Result<HeaderPairs, String> {
    let spec = spec.trim();
    if spec.is_empty() {
        return Ok(Vec::new());
    }

    let value: Value = serde_json::from_str(spec)
        .map_err(|e| format!("must be a JSON object of header name -> value: {e}"))?;
    let object = value.as_object().ok_or_else(|| {
        "must be a JSON object of header name -> value (e.g. {\"x-api-key\":\"…\"})".to_string()
    })?;

    let mut headers = Vec::with_capacity(object.len());
    for (name, value) in object {
        let value = value
            .as_str()
            .ok_or_else(|| format!("header {name:?} must have a string value"))?;
        validate(name, value)?;
        headers.push((name.clone(), value.to_string()));
    }
    Ok(headers)
}

/// Validate one header pair. Error messages name the header but never echo
/// its value, since that value is a credential.
pub fn validate(name: &str, value: &str) -> Result<(), String> {
    if name.is_empty() || !name.bytes().all(is_tchar) {
        return Err(format!(
            "header name {name:?} is not a valid HTTP token (letters, digits and !#$%&'*+-.^_`|~ only)"
        ));
    }
    if value
        .bytes()
        .any(|b| b == b'\r' || b == b'\n' || b == 0 || b == 0x7f)
    {
        return Err(format!(
            "header {name:?} contains a control character, which is not allowed in a header value"
        ));
    }
    Ok(())
}

/// `tchar` per RFC 7230 §3.2.6.
fn is_tchar(byte: u8) -> bool {
    byte.is_ascii_alphanumeric() || b"!#$%&'*+-.^_`|~".contains(&byte)
}

/// Apply `overrides` on top of `base`, replacing a same-named header
/// (case-insensitively) rather than sending it twice.
pub fn merge(base: HeaderPairs, overrides: HeaderPairs) -> HeaderPairs {
    let mut merged = base;
    for (name, value) in overrides {
        match merged
            .iter_mut()
            .find(|(existing, _)| existing.eq_ignore_ascii_case(&name))
        {
            Some(existing) => existing.1 = value,
            None => merged.push((name, value)),
        }
    }
    merged
}

/// Header names only — the loggable form of a header set.
pub fn redact(headers: &[(String, String)]) -> Vec<&str> {
    headers.iter().map(|(name, _)| name.as_str()).collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_a_json_object_of_headers() {
        let headers = parse(r#"{"Authorization":"Bearer abc","x-api-key":"k1"}"#).unwrap();
        assert_eq!(headers.len(), 2);
        assert!(headers
            .iter()
            .any(|(n, v)| n == "Authorization" && v == "Bearer abc"));
    }

    #[test]
    fn empty_spec_means_no_headers() {
        assert!(parse("").unwrap().is_empty());
        assert!(parse("   ").unwrap().is_empty());
    }

    #[test]
    fn value_may_contain_url_and_comma_characters() {
        let headers = parse(r#"{"x-token":"a,b:c=d/e+f"}"#).unwrap();
        assert_eq!(headers[0].1, "a,b:c=d/e+f");
    }

    #[test]
    fn rejects_non_json_and_non_objects() {
        assert!(parse("x-api-key=abc").is_err());
        assert!(parse(r#"["x"]"#).is_err());
    }

    #[test]
    fn rejects_non_string_values() {
        let err = parse(r#"{"x-api-key":42}"#).unwrap_err();
        assert!(err.contains("x-api-key"));
    }

    #[test]
    fn rejects_invalid_header_names() {
        assert!(parse(r#"{"bad name":"v"}"#).is_err());
        assert!(parse(r#"{"bad:name":"v"}"#).is_err());
    }

    #[test]
    fn rejects_control_characters_in_values() {
        let err = parse("{\"x-api-key\":\"a\\nb\"}").unwrap_err();
        assert!(err.contains("x-api-key"));
        // The value itself must never be echoed.
        assert!(!err.contains("a\nb"));
    }

    #[test]
    fn merge_overrides_case_insensitively() {
        let merged = merge(
            vec![("authorization".to_string(), "old".to_string())],
            vec![("Authorization".to_string(), "new".to_string())],
        );
        assert_eq!(merged.len(), 1);
        assert_eq!(merged[0].1, "new");
    }

    #[test]
    fn redact_returns_names_only() {
        let headers = vec![("x-api-key".to_string(), "secret".to_string())];
        assert_eq!(redact(&headers), vec!["x-api-key"]);
    }

    #[test]
    fn redact_never_contains_values() {
        let headers = vec![("Authorization".to_string(), "Bearer secret".to_string())];
        let rendered = format!("{:?}", redact(&headers));
        assert!(!rendered.contains("secret"));
    }
}

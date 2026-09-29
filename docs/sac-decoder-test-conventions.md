# SAC Decoder Unit Testing Specification

## Overview

This specification details unit testing guidelines and coverage standards for Stellar Asset Contract (SAC) event decoding within the Trident indexer engine (`crates/indexer/src/parser/sac.rs` and `crates/indexer/src/parser/token_events.rs`).

---

## Test Coverage Requirements

Unit tests for SAC event parsing must cover three primary operational domains:

1. **Registry Resolution (`SacRegistry`)**:
   - Verification of contract address to asset code mappings (AlphaNum4, AlphaNum12, and Native XLM).
   - Graceful fallback and error propagation when encountering unregistered contract addresses.

2. **XDR Payload Decoding**:
   - Validation of binary XDR topics and data payloads representing SEP-41 token events (`transfer`, `mint`, `burn`, `clawback`).
   - Boundary condition testing for malformed or truncated XDR input streams.

3. **Asset Context Attachment**:
   - Verification that decoded event domain objects accurately preserve asset symbol names, issuer public keys, and numeric balance adjustments.

---

## Example Unit Test Architecture (`crates/indexer/src/parser/sac.rs`)

```rust
#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_sac_registry_lookup_success() {
        let registry = SacRegistry::default();
        let known_address = "CCW67TSBXS2XYO7M5DYH6Y5CD226LY4JJZFKEWZ452GYBZWFZCC25ZAX";
        let asset = registry.lookup(known_address);
        assert!(asset.is_some());
    }

    #[test]
    fn test_sac_registry_lookup_unknown_contract() {
        let registry = SacRegistry::default();
        let unknown_address = "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA";
        let asset = registry.lookup(unknown_address);
        assert!(asset.is_none());
    }

    #[test]
    fn test_malformed_xdr_payload_handling() {
        let malformed_bytes = vec![0x00, 0xff, 0x12, 0x34];
        let result = parse_sac_event(&malformed_bytes);
        assert!(result.is_err());
    }
}
```

---

## Execution Instructions

Run unit tests via `cargo`:

```bash
cargo test --package trident-indexer --lib parser::sac
```

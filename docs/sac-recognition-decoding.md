# SAC Recognition & Decoding Specification

## Overview

This specification details Stellar Asset Contract (SAC) recognition, contract address resolution, and SEP-41 token event decoding within the Trident indexer engine (`crates/indexer/src/parser/sac.rs`).

---

## Architectural Principles

Stellar Asset Contracts (SAC) enable classic Stellar assets (Native XLM, AlphaNum4, and AlphaNum12) to be utilized seamlessly within Soroban smart contracts.

### Core Modules

1. **`SacRegistry` (`crates/indexer/src/parser/sac.rs`)**:
   - Maintains an in-memory mapping between Soroban contract addresses (`C...`) and their underlying Stellar asset descriptors.
   - Initialized via `Parser::with_sac_registry` and updated dynamically as new asset wrapper contracts are deployed.

2. **SEP-41 Event Parser (`crates/indexer/src/parser/token_events.rs`)**:
   - Decodes binary XDR payloads for standard token interface events:
     - `transfer(from, to, amount)`
     - `mint(to, amount)`
     - `burn(from, amount)`
     - `clawback(from, amount)`

---

## Extension Guidelines

To extend SAC decoding capabilities for additional custom token standards:

1. Define new event topic signatures in `crates/indexer/src/parser/sac.rs`.
2. Implement binary XDR data deserializers adhering to the `SacDecoder` trait.
3. Add unit tests under `crates/indexer/src/parser/sac.rs::tests`.

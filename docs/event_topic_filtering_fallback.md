# Degraded-Filtering Fallback Path

## Overview

This document outlines the architecture and rationale for the client-side event filtering mechanism within Trident (`crates/indexer/src/streamer/mod.rs`). It acts as a safety boundary for edge cases where server-side RPC pushdown filtering is bypassed, degraded, or ignored.

---

## Primary Flow: RPC Server-Side Pushdown

Under standard operational conditions, filtering is offloaded entirely to the Soroban RPC node:

1. The indexer reads the contract allowlist and topic constraints.
2. `crates/indexer/src/rpc/filters.rs::build_event_filters()` compiles these constraints into a `FilterPlan`.
3. The `FilterPlan` is serialized into the `filters` array within the `getEvents` RPC payload.
4. The RPC node processes the request, returning only the targeted events.

In this scenario, Trident receives a minimal, pre-filtered payload, meaning that extensive in-memory filtering scans are structurally avoided on the hot path.

---

## The Correctness Boundary: Client-Side Fallback

While the primary flow relies on RPC-side pushdown, `crates/indexer/src/streamer/mod.rs` maintains a deliberate client-side filtering check (the "Degraded-Filtering Fallback Path"). 

### Why is this necessary?

1. **RPC Non-Compliance**: Certain RPC node implementations (or older versions) may ignore complex filter arrays or process them incorrectly, returning broader event sets than requested.
2. **Filter Pagination Degradation**: When paginating over large historical ledger ranges, RPC nodes may fall back to returning unfiltered blocks to avoid timeout execution limits on complex topic intersections.
3. **Complex Matching Constraints**: Not all logical intersections (e.g., specific regex patterns or dynamic wildcard combinations) can be natively expressed in the Soroban `getEvents` filter schema. 

### Implementation Rationale

The client-side scan serves as a **correctness boundary**. If the RPC returns unrequested events, this mechanism guarantees that downstream systems (Postgres ingest, Redis Pub/Sub, gRPC fanout) are never polluted with invalid data.

Replacing this linear scan with an exact-match `HashSet` would fundamentally break this fallback mechanism, as hash lookups cannot express wildcards, prefix matching, or complex address filter structures. Because the hot path is already optimized via Server-Side Pushdown, the O(N·M) cost of this fallback scan is negligible, only activating as a resilient safety net for degraded RPC responses.

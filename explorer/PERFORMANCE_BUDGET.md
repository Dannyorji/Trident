# Explorer Performance Budget

This document defines performance targets for the Soroban Event Explorer against realistic testnet data volumes. These budgets are measured against a testnet database with real event data (not fixtures).

## Targets

All measurements assume **realistic testnet volumes** (10,000+ events per contract, 100,000+ total events).

### Page Load (Time to First Byte + Render)

| Route | Target | Notes |
|-------|--------|-------|
| `/` (Home) | ≤ 1.0s FCP, ≤ 2.0s LCP | Includes 10 recent events + ticker JS |
| `/contract/:address` | ≤ 1.5s FCP, ≤ 3.0s LCP | Includes 25 paginated events + filters |
| `/contract/:address/event/:id` | ≤ 1.0s FCP, ≤ 2.0s LCP | Single event detail |

### Time to First Meaningful Paint (TTFMP)

| Route | Target | Notes |
|-------|--------|-------|
| `/` | ≤ 1.2s | Hero section + search form visible |
| `/contract/:address` | ≤ 1.8s | Contract header + event table header visible |
| `/contract/:address/event/:id` | ≤ 1.2s | Event detail header visible |

### Query Latency (API)

| Endpoint | Target | Notes |
|----------|--------|-------|
| `GET /v1/events` (home ticker) | ≤ 300ms p50, ≤ 500ms p95 | 10 recent events, no filter |
| `GET /v1/events` (contract list) | ≤ 500ms p50, ≤ 800ms p95 | 25 events with contractId filter |
| `GET /v1/events/:id` | ≤ 200ms p50, ≤ 400ms p95 | Single event lookup |

### Server Response Time (SSR)

| Route | Target | Notes |
|-------|--------|-------|
| Home | ≤ 600ms | Astro SSR + 2 API calls (recent events + ticker) |
| Contract listing | ≤ 800ms | Astro SSR + 1 API call (events with filter) |
| Event detail | ≤ 600ms | Astro SSR + 1 API call (single event) |

## Measurement Strategy

1. **Baseline**: Measure all routes on testnet against real data (min 10k events/contract)
2. **Frequency**: Run weekly via scheduled test or on-demand before releases
3. **Tools**: 
   - Lighthouse CI for Core Web Vitals (FCP, LCP)
   - Custom query latency tests via `npm run perf-test`
   - API timing instrumentation in production logs
4. **Threshold**: Any regression > 10% triggers CI failure

## Known Bottlenecks

- **Home ticker polling**: Fetches 10 events every 5s client-side. At scale, consider caching or reducing frequency.
- **Contract listing with many events**: Large tables render slowly. Consider virtual scrolling or reducing default page size.
- **Distinct topic filter**: Computed from all events on page load. Pre-compute or cache for contracts with 1000+ events.

## Remediation

If a budget is exceeded:
1. Profile the slow endpoint (Chrome DevTools, server logs)
2. Check API query latency vs rendering
3. If API slow: optimize query filters (add indexes, pagination limits)
4. If rendering slow: reduce data payload, defer non-critical fields, or paginate
5. If SSR slow: cache responses, reduce upstream calls, or use streaming


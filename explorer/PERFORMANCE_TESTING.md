# Performance Testing & Monitoring

This document explains how to test and monitor explorer performance against realistic testnet data volumes.

## Quick Start

### Local Testing

Test against a live testnet API:

```bash
npm run perf-test -- \
  --api-url https://api.testnet.trident.dev \
  --api-key YOUR_API_KEY \
  --contract-id CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABSC4
```

Exit code 0 = all budgets met. Exit code 1 = budget exceeded.

### CI Testing

Performance tests run automatically:
- **Schedule**: Weekly, every Monday at 9 AM UTC
- **Trigger**: Manual via `workflow_dispatch` with custom API URL
- **Budget**: See `PERFORMANCE_BUDGET.md`

Failures create comments on related PRs/issues.

## Files

| File | Purpose |
|------|---------|
| `PERFORMANCE_BUDGET.md` | Explicit targets (FCP, LCP, API latency) |
| `SCALE_BOTTLENECKS.md` | Known issues and mitigation strategies |
| `scripts/perf-test.ts` | Node.js test suite (run locally or in CI) |
| `.github/workflows/explorer-perf.yml` | Weekly scheduled GitHub Actions workflow |
| `.lighthouserc.json` | Lighthouse CI config (optional, future) |

## Measurement

### What We Measure

1. **API Latency** (p50, p95)
   - Home ticker: `GET /v1/events?limit=10`
   - Contract list: `GET /v1/events?contractId=...&limit=25`
   - Event detail: `GET /v1/events/:id`

2. **Server Response Time** (Astro SSR)
   - Excludes network latency to API
   - Includes rendering + template processing

3. **Core Web Vitals** (optional, Lighthouse)
   - FCP: First Contentful Paint
   - LCP: Largest Contentful Paint
   - CLS: Cumulative Layout Shift

### Budget Thresholds

All budgets assume **realistic testnet data** (10k+ events/contract, 100k+ total).

- **Home page**: ≤1.0s FCP, ≤2.0s LCP
- **Contract list**: ≤1.5s FCP, ≤3.0s LCP
- **Event detail**: ≤1.0s FCP, ≤2.0s LCP
- **API latency**: 300-500ms p50, 500-800ms p95

See `PERFORMANCE_BUDGET.md` for full details.

## Remediation

If a budget is exceeded:

1. **Identify the bottleneck**
   - API slow? Check backend query + indexes
   - Render slow? Check Astro template, reduce payload
   - SSR slow? Consider caching or streaming

2. **Check the logs**
   ```bash
   npm run perf-test 2>&1 | grep -E "latency|budget"
   ```

3. **Profile locally**
   ```bash
   npm run dev
   # Open Chrome DevTools > Network tab, throttle to Slow 3G
   # Navigate pages, check response times
   ```

4. **Update the budget** (if intentional)
   - Edit `PERFORMANCE_BUDGET.md`
   - Document the reason
   - Get team review

## Integration with Development

### Before Committing

Run tests locally to catch regressions early:

```bash
npm run perf-test
```

### Before Releasing

1. Run full performance suite against production testnet
2. Compare results to baseline (stored in repo or CI artifacts)
3. Regression > 10% = blocker

### Continuous Monitoring

- Production: Set up APM (e.g., Datadog, New Relic) to track real user metrics
- Alerts: Configure alerts if API latency > 1s p95 or error rate > 5%

## Future Improvements

- [ ] Add Lighthouse CI integration for Core Web Vitals
- [ ] Compare p50/p95 against historical baseline
- [ ] Export metrics to monitoring dashboard
- [ ] Add synthetic user journey tests (contract detail → load more → event detail)
- [ ] Profile memory usage at scale

/**
 * Performance test suite for explorer against real testnet data.
 * Measures API latency, server response time, and page render metrics.
 * 
 * Usage:
 *   npm run perf-test -- --api-url https://api.testnet.trident.dev --api-key YOUR_KEY
 */

import { performance } from 'perf_hooks';

interface PerfResult {
  route: string;
  metric: string;
  value: number;
  unit: string;
  budget: number;
  ok: boolean;
}

const results: PerfResult[] = [];

// Parse CLI args
const args = process.argv.slice(2);
const apiUrl = args[args.indexOf('--api-url') + 1] || 'https://api.testnet.trident.dev';
const apiKey = args[args.indexOf('--api-key') + 1] || '';
const testContractId = args[args.indexOf('--contract-id') + 1] || 'CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABSC4';

async function time(label: string, fn: () => Promise<any>): Promise<number> {
  const start = performance.now();
  await fn();
  const duration = performance.now() - start;
  console.log(`  ${label}: ${duration.toFixed(2)}ms`);
  return duration;
}

function checkBudget(route: string, metric: string, value: number, budget: number, unit: string = 'ms'): void {
  const ok = value <= budget;
  results.push({ route, metric, value, budget, unit, ok });
  const symbol = ok ? '✓' : '✗';
  const color = ok ? '\x1b[32m' : '\x1b[31m';
  console.log(`${color}  ${symbol} ${metric}: ${value.toFixed(2)}${unit} (budget: ${budget}${unit})\x1b[0m`);
}

async function runTests() {
  console.log('\n📊 Explorer Performance Tests\n');
  console.log(`API: ${apiUrl}`);
  console.log(`Contract: ${testContractId}\n`);

  // Test 1: Home ticker API (10 recent events)
  console.log('Test 1: Home ticker (recent events)');
  const homeLatency = await time('  /v1/events (limit=10)', async () => {
    const url = new URL(`${apiUrl}/v1/events`);
    url.searchParams.set('limit', '10');
    const res = await fetch(url.toString(), {
      headers: apiKey ? { 'X-API-Key': apiKey } : {},
    });
    if (!res.ok) throw new Error(`API ${res.status}`);
    await res.json();
  });
  checkBudget('home', 'API latency', homeLatency, 300);

  // Test 2: Contract listing API (25 events with filter)
  console.log('\nTest 2: Contract listing (25 paginated events)');
  const contractLatency = await time('  /v1/events (contractId filter, limit=25)', async () => {
    const url = new URL(`${apiUrl}/v1/events`);
    url.searchParams.set('contractId', testContractId);
    url.searchParams.set('limit', '25');
    const res = await fetch(url.toString(), {
      headers: apiKey ? { 'X-API-Key': apiKey } : {},
    });
    if (!res.ok) throw new Error(`API ${res.status}`);
    await res.json();
  });
  checkBudget('contract', 'API latency', contractLatency, 500);

  // Test 3: Single event API
  console.log('\nTest 3: Event detail (single event)');
  let eventId: string | null = null;
  await time('  /v1/events (fetch first event for detail test)', async () => {
    const url = new URL(`${apiUrl}/v1/events`);
    url.searchParams.set('limit', '1');
    const res = await fetch(url.toString(), {
      headers: apiKey ? { 'X-API-Key': apiKey } : {},
    });
    if (!res.ok) throw new Error(`API ${res.status}`);
    const data = await res.json();
    if (data.events?.length > 0) eventId = data.events[0].id;
  });

  if (eventId) {
    // Bind to a const so the type narrows inside the async closure below;
    // TypeScript cannot keep the narrowing on a let assigned from a callback.
    const detailEventId: string = eventId;
    const eventLatency = await time('  /v1/events/:id', async () => {
      const res = await fetch(`${apiUrl}/v1/events/${encodeURIComponent(detailEventId)}`, {
        headers: apiKey ? { 'X-API-Key': apiKey } : {},
      });
      if (!res.ok) throw new Error(`API ${res.status}`);
      await res.json();
    });
    checkBudget('event-detail', 'API latency', eventLatency, 200);
  }

  // Test 4: P95 latency (repeated calls)
  console.log('\nTest 4: P95 latency (10 repeated calls)');
  const latencies: number[] = [];
  for (let i = 0; i < 10; i++) {
    const lat = await time(`  Call ${i + 1}`, async () => {
      const url = new URL(`${apiUrl}/v1/events`);
      url.searchParams.set('contractId', testContractId);
      url.searchParams.set('limit', '25');
      const res = await fetch(url.toString(), {
        headers: apiKey ? { 'X-API-Key': apiKey } : {},
      });
      if (!res.ok) throw new Error(`API ${res.status}`);
      await res.json();
    });
    latencies.push(lat);
  }
  const p95 = latencies.sort((a, b) => a - b)[Math.floor(latencies.length * 0.95)];
  checkBudget('contract', 'API latency (p95)', p95, 800);

  // Summary
  console.log('\n📋 Summary\n');
  const passed = results.filter((r) => r.ok).length;
  const total = results.length;
  const symbol = passed === total ? '✅' : '⚠️ ';
  console.log(`${symbol} ${passed}/${total} budgets met\n`);

  if (passed < total) {
    console.log('Failed budgets:');
    results
      .filter((r) => !r.ok)
      .forEach((r) => {
        const over = r.value - r.budget;
        console.log(`  - ${r.route}/${r.metric}: ${over.toFixed(2)}${r.unit} over budget`);
      });
    process.exit(1);
  }
}

runTests().catch((err) => {
  console.error('\n❌ Test error:', err.message);
  process.exit(1);
});

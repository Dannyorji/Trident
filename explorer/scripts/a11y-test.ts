/**
 * Accessibility test suite for explorer.
 * Tests keyboard navigation, screen reader labels, and WCAG compliance.
 * 
 * Usage:
 *   npm run a11y-test -- --url http://localhost:3000
 */

import { chromium } from '@playwright/test';
import { injectAxe, getViolations } from 'axe-playwright';

interface A11yResult {
  page: string;
  passed: boolean;
  violations: Array<{
    id: string;
    impact: string;
    nodes: number;
  }>;
}

const results: A11yResult[] = [];

async function testPage(browser: any, url: string, name: string) {
  console.log(`\nTesting: ${name}`);
  const page = await browser.newPage();

  try {
    await page.goto(url, { waitUntil: 'networkidle' });
    await injectAxe(page);
    // getViolations returns the violation list; checkA11y returns void and
    // throws instead, so it cannot be used to collect results.
    const violations = await getViolations(page);

    if (violations.length > 0) {
      console.log(`  ✗ Found accessibility violations`);
      results.push({
        page: name,
        passed: false,
        violations: violations.map((v: any) => ({
          id: v.id,
          impact: v.impact,
          nodes: v.nodes.length,
        })),
      });
    } else {
      console.log(`  ✓ No violations found`);
      results.push({
        page: name,
        passed: true,
        violations: [],
      });
    }
  } catch (err: any) {
    console.log(`  ✗ Test error: ${err.message}`);
    results.push({
      page: name,
      passed: false,
      violations: [{ id: 'error', impact: 'critical', nodes: 1 }],
    });
  } finally {
    await page.close();
  }
}

async function runTests() {
  const args = process.argv.slice(2);
  const baseUrl = args[args.indexOf('--url') + 1] || 'http://localhost:3000';

  console.log('\n📋 Explorer Accessibility Tests\n');
  console.log(`Base URL: ${baseUrl}\n`);

  const browser = await chromium.launch();

  try {
    // Test key pages
    await testPage(browser, baseUrl, 'Home');
    await testPage(
      browser,
      `${baseUrl}/contract/CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABSC4`,
      'Contract List'
    );
    await testPage(
      browser,
      `${baseUrl}/contract/CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABSC4/event/test-event-id`,
      'Event Detail'
    );

    // Summary
    console.log('\n📊 Summary\n');
    const passed = results.filter((r) => r.passed).length;
    const total = results.length;
    const symbol = passed === total ? '✅' : '⚠️ ';
    console.log(`${symbol} ${passed}/${total} pages passed\n`);

    if (passed < total) {
      console.log('Failed pages:\n');
      results
        .filter((r) => !r.passed)
        .forEach((r) => {
          console.log(`  ${r.page}:`);
          const byImpact = r.violations.reduce(
            (acc, v) => {
              acc[v.impact] = (acc[v.impact] || 0) + 1;
              return acc;
            },
            {} as Record<string, number>
          );
          Object.entries(byImpact).forEach(([impact, count]) => {
            console.log(`    ${impact}: ${count} violation(s)`);
          });
        });
      process.exit(1);
    }
  } finally {
    await browser.close();
  }
}

runTests().catch((err) => {
  console.error('\n❌ Test error:', err.message);
  process.exit(1);
});

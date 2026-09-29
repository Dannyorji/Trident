// Verifies one API key cannot exceed its configured tier limit when a
// launch-scale burst arrives concurrently.
//
// Usage:
//   BASE_URL=https://staging.example.com API_KEY=<key> EXPECTED_LIMIT=100 \
//     CONCURRENT_REQUESTS=1000 k6 run load-tests/rate-limit-concurrency.js
//
// API_KEY must be a valid key and EXPECTED_LIMIT must match that key's tier.

import http from "k6/http";
import { check } from "k6";
import { Counter } from "k6/metrics";

const BASE_URL = __ENV.BASE_URL || "http://localhost:3000";
const API_KEY = __ENV.API_KEY || "";
const EXPECTED_LIMIT = Number(__ENV.EXPECTED_LIMIT || 100);
const CONCURRENT_REQUESTS = Number(__ENV.CONCURRENT_REQUESTS || 1000);

if (!API_KEY) {
  throw new Error("API_KEY is required to exercise the per-key rate limiter");
}
if (EXPECTED_LIMIT < 1 || CONCURRENT_REQUESTS <= EXPECTED_LIMIT) {
  throw new Error("set EXPECTED_LIMIT >= 1 and CONCURRENT_REQUESTS > EXPECTED_LIMIT");
}

const allowed = new Counter("rate_limit_allowed");
const rejected = new Counter("rate_limit_rejected");
const unexpected = new Counter("rate_limit_unexpected");

export const options = {
  scenarios: {
    concurrentBurst: {
      executor: "shared-iterations",
      vus: 1,
      iterations: 1,
      maxDuration: "30s",
    },
  },
  batch: CONCURRENT_REQUESTS,
  batchPerHost: CONCURRENT_REQUESTS,
  thresholds: {
    rate_limit_allowed: [`count<=${EXPECTED_LIMIT}`],
    rate_limit_rejected: [
      `count>=${CONCURRENT_REQUESTS - EXPECTED_LIMIT}`,
    ],
    rate_limit_unexpected: ["count==0"],
  },
};

export default function () {
  const request = {
    method: "GET",
    url: `${BASE_URL}/v1/events?limit=1`,
    params: { headers: { "X-API-Key": API_KEY } },
  };
  const responses = http.batch(
    Array.from({ length: CONCURRENT_REQUESTS }, () => request),
  );

  for (const response of responses) {
    if (response.status === 200) {
      allowed.add(1);
      check(response, {
        "allowed response has tier limit": (r) =>
          r.headers["X-Ratelimit-Limit"] === String(EXPECTED_LIMIT),
        "allowed response has remaining count": (r) =>
          r.headers["X-Ratelimit-Remaining"] !== undefined,
      });
    } else if (response.status === 429) {
      rejected.add(1);
      check(response, {
        "rejected response has zero remaining": (r) =>
          r.headers["X-Ratelimit-Remaining"] === "0",
        "rejected response has Retry-After": (r) =>
          r.headers["Retry-After"] !== undefined,
      });
    } else {
      unexpected.add(1);
    }
  }
}

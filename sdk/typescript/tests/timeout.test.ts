import { afterEach, describe, expect, it, vi } from "vitest";
import { DEFAULT_TIMEOUT_MS, TridentClient, TridentError } from "../src/index.js";

// Issue #520 follow-up: the explorer's move onto the SDK dropped a 30s
// AbortController timeout, and the SDK had none of its own — `fetch` never
// times out on its own, so a stalled API held callers open indefinitely and
// the retry layer never saw a rejection to react to.

const BASE_URL = "http://localhost:3000";
const API_KEY = "test-key";

const mockEvent = {
  id: "00000000-0000-0000-0000-000000000001",
  contract_id: "CTEST",
  ledger_sequence: 100,
  ledger_timestamp: "2024-01-01T00:00:00Z",
  transaction_hash: "abc123",
  event_index: 0,
  event_type: "contract",
  topics: ["transfer"],
  data: '"null"',
  created_at: "2024-01-01T00:00:00Z",
};

function jsonResponse(body: unknown, status = 200) {
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: { get: () => null },
    json: () => Promise.resolve(body),
    text: () => Promise.resolve(JSON.stringify(body)),
  };
}

/**
 * A fetch that never settles until the caller's AbortSignal fires, then
 * rejects the way a real runtime does. Without a timeout wired in, a request
 * against this hangs forever — which is exactly the regression under test.
 */
function hangingFetch() {
  return vi.fn((_url: string, init?: { signal?: AbortSignal }) => {
    return new Promise((_resolve, reject) => {
      const signal = init?.signal;
      if (!signal) return; // no timeout wired: hang forever
      if (signal.aborted) {
        reject(Object.assign(new Error("aborted"), { name: "AbortError" }));
        return;
      }
      signal.addEventListener("abort", () => {
        reject(Object.assign(new Error("aborted"), { name: "AbortError" }));
      });
    });
  });
}

function client(overrides: Record<string, unknown> = {}) {
  return new TridentClient({
    apiUrl: BASE_URL,
    apiKey: API_KEY,
    network: "testnet",
    retry: false,
    ...overrides,
  });
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe("request timeout", () => {
  it("aborts a hung request and reports TIMEOUT rather than hanging", async () => {
    const fetchMock = hangingFetch();
    vi.stubGlobal("fetch", fetchMock);

    const err = await client({ timeoutMs: 20 })
      .queryEvents({ contractId: "CTEST" })
      .then(
        () => null,
        (e: unknown) => e,
      );

    expect(err).toBeInstanceOf(TridentError);
    expect((err as TridentError).code).toBe("TIMEOUT");
    expect((err as TridentError).message).toContain("20ms");
  });

  it("passes an AbortSignal to fetch by default", async () => {
    const fetchMock = vi.fn(() =>
      Promise.resolve(jsonResponse({ events: [mockEvent], next_cursor: null, has_more: false })),
    );
    vi.stubGlobal("fetch", fetchMock);

    await client().queryEvents({ contractId: "CTEST" });

    const init = fetchMock.mock.calls[0]?.[1] as { signal?: AbortSignal } | undefined;
    expect(init?.signal).toBeInstanceOf(AbortSignal);
    expect(init?.signal?.aborted).toBe(false);
  });

  it("omits the signal when the timeout is disabled", async () => {
    const fetchMock = vi.fn(() =>
      Promise.resolve(jsonResponse({ events: [mockEvent], next_cursor: null, has_more: false })),
    );
    vi.stubGlobal("fetch", fetchMock);

    await client({ timeoutMs: false }).queryEvents({ contractId: "CTEST" });

    const init = fetchMock.mock.calls[0]?.[1] as { signal?: AbortSignal } | undefined;
    expect(init?.signal).toBeUndefined();
  });

  it("lets a per-call timeout override the client default", async () => {
    const fetchMock = hangingFetch();
    vi.stubGlobal("fetch", fetchMock);

    const err = await client({ timeoutMs: 50_000 })
      .queryEvents({ contractId: "CTEST" }, { timeoutMs: 15 })
      .then(
        () => null,
        (e: unknown) => e,
      );

    expect((err as TridentError).code).toBe("TIMEOUT");
    expect((err as TridentError).message).toContain("15ms");
  });

  it("gives each retry attempt a fresh signal instead of reusing an aborted one", async () => {
    const fetchMock = hangingFetch();
    vi.stubGlobal("fetch", fetchMock);

    const err = await client({
      timeoutMs: 10,
      retry: { maxAttempts: 3, initialDelayMs: 1, maxDelayMs: 2, maxTotalWaitMs: 1_000 },
    })
      .queryEvents({ contractId: "CTEST" })
      .then(
        () => null,
        (e: unknown) => e,
      );

    expect((err as TridentError).code).toBe("TIMEOUT");
    expect(fetchMock).toHaveBeenCalledTimes(3);
    // Each attempt must carry its own controller — a reused aborted signal
    // would make attempts 2 and 3 fail instantly rather than being retried.
    const signals = fetchMock.mock.calls.map(
      (c) => (c[1] as { signal?: AbortSignal } | undefined)?.signal,
    );
    expect(new Set(signals).size).toBe(3);
  });

  it("does not time out a request that responds in time", async () => {
    const fetchMock = vi.fn(() =>
      Promise.resolve(jsonResponse({ events: [mockEvent], next_cursor: null, has_more: false })),
    );
    vi.stubGlobal("fetch", fetchMock);

    const page = await client({ timeoutMs: 5_000 }).queryEvents({ contractId: "CTEST" });
    expect(page.events).toHaveLength(1);
  });

  it("rejects a nonsensical timeout instead of aborting immediately", async () => {
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse({ events: [], next_cursor: null, has_more: false })));
    vi.stubGlobal("fetch", fetchMock);

    await expect(
      client({ timeoutMs: 0 }).queryEvents({ contractId: "CTEST" }),
    ).rejects.toThrow(/positive number or false/);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("defaults to 30s", () => {
    expect(DEFAULT_TIMEOUT_MS).toBe(30_000);
  });
});

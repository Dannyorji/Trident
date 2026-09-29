import type { Network } from './types';

const TESTNET_RPC =
  import.meta.env.TRIDENT_TESTNET_SOROBAN_RPC_URL ?? 'https://soroban-testnet.stellar.org';
const MAINNET_RPC =
  import.meta.env.TRIDENT_MAINNET_SOROBAN_RPC_URL ?? 'https://mainnet.sorobanrpc.com';

const PROBE_TIMEOUT_MS = 2500;
// How many recent ledgers to scan for on-chain events when deciding whether a
// contract is "not indexed yet" vs "no events yet". ~1000 ledgers ≈ 1.5h at
// Stellar's ~5s close time, which covers freshly deployed contracts.
const PROBE_LEDGER_WINDOW = 1000;
// Short TTL so an empty contract's page doesn't hammer the public RPC on every
// reload, but the state still recovers quickly once the contract goes live.
const CACHE_TTL_MS = 60_000;

export type OnChainProbeResult =
  | { status: 'has_events' }
  | { status: 'no_events' }
  | { status: 'invalid_contract' }
  | { status: 'inconclusive' };

interface ProbeCacheEntry {
  result: OnChainProbeResult;
  at: number;
}

const probeCache = new Map<string, ProbeCacheEntry>();

function rpcUrl(network: Network): string {
  return network === 'mainnet' ? MAINNET_RPC : TESTNET_RPC;
}

interface RpcError extends Error {
  rpcCode?: number;
}

async function rpcPost<T>(url: string, method: string, params: unknown): Promise<T> {
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), PROBE_TIMEOUT_MS);
  try {
    const res = await fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
      body: JSON.stringify({ jsonrpc: '2.0', id: 1, method, params }),
      signal: controller.signal,
    });
    if (!res.ok) throw new Error(`RPC HTTP ${res.status}`);
    const body = (await res.json()) as {
      result?: T;
      error?: { code?: number; message?: string };
    };
    if (body.error) {
      const err: RpcError = new Error(body.error.message);
      err.rpcCode = body.error.code;
      throw err;
    }
    return body.result as T;
  } finally {
    clearTimeout(timeout);
  }
}

async function runProbe(network: Network, contractId: string): Promise<OnChainProbeResult> {
  try {
    const latest = await rpcPost<{ sequence: number }>(rpcUrl(network), 'getLatestLedger', {});
    if (!latest || typeof latest.sequence !== 'number') return { status: 'inconclusive' };

    const seq = latest.sequence;
    const startLedger = Math.max(1, seq - PROBE_LEDGER_WINDOW);

    const result = await rpcPost<{ events?: unknown[] }>(rpcUrl(network), 'getEvents', {
      startLedger,
      endLedger: seq,
      filters: [{ type: 'contract', contractIds: [contractId] }],
      limit: 1,
    });

    if (Array.isArray(result?.events)) {
      return result.events.length > 0 ? { status: 'has_events' } : { status: 'no_events' };
    }
    return { status: 'inconclusive' };
  } catch (err) {
    const rpcCode = (err as RpcError)?.rpcCode;
    const message = err instanceof Error ? err.message : '';
    if (rpcCode === -32602 && /contract ID .*invalid/i.test(message)) {
      // The strkey checksum failed: this is not a real contract address.
      return { status: 'invalid_contract' };
    }
    // RPC unreachable or degraded — we can't tell; callers fall back to the
    // honest "no events" interpretation.
    return { status: 'inconclusive' };
  }
}

/**
 * Best-effort on-chain check that tells the explorer whether a contract with
 * zero Trident events is simply quiet ("no events yet") or actually emitting
 * events on the Stellar network that Trident hasn't indexed ("not indexed").
 *
 * Never throws: every failure path returns `inconclusive` so the caller can
 * render an honest fallback state.
 */
export async function probeContractOnChain(
  network: Network,
  contractId: string,
): Promise<OnChainProbeResult> {
  const key = `${network}:${contractId}`;
  const cached = probeCache.get(key);
  if (cached && Date.now() - cached.at < CACHE_TTL_MS) return cached.result;

  const result = await runProbe(network, contractId);
  probeCache.set(key, { result, at: Date.now() });
  return result;
}
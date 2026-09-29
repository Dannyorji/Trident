import type { SorobanEvent, ListEventsResponse, Network } from "./types";

const TESTNET_URL =
  import.meta.env.TRIDENT_TESTNET_API_URL ?? "https://api.testnet.trident.dev";
const MAINNET_URL =
  import.meta.env.TRIDENT_MAINNET_API_URL ?? "https://api.mainnet.trident.dev";
const API_KEY: string = import.meta.env.EXPLORER_API_KEY ?? "";
const API_TIMEOUT = 30000; // 30 second timeout

function baseUrl(network: Network): string {
  return network === "mainnet" ? MAINNET_URL : TESTNET_URL;
}

function authHeaders(): HeadersInit {
  const h: Record<string, string> = {};
  if (API_KEY) h["X-API-Key"] = API_KEY;
  return h;
}

/**
 * Typed error for a non-OK Trident API response. `code` is the machine
 * readable code from the standard {"error":{code,message}} envelope so callers
 * can surface a deliberate state instead of a raw error string.
 */
export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly requestId?: string;

  constructor(status: number, code: string, message: string, requestId?: string) {
    super(message || `Request failed (HTTP ${status})`);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.requestId = requestId;
  }
}

export interface QueryEventsParams {
  contractId?: string;
  topic0?: string;
  ledgerFrom?: number;
  ledgerTo?: number;
  cursor?: string;
  limit?: number;
  network?: Network;
}

async function fetchWithTimeout(
  url: string,
  options: RequestInit = {},
  timeoutMs = API_TIMEOUT,
): Promise<Response> {
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), timeoutMs);

  try {
    const res = await fetch(url, {
      ...options,
      signal: controller.signal,
    });
    return res;
  } finally {
    clearTimeout(timeout);
  }
}

/**
 * Fetch a JSON body and throw an {@link ApiError} on any non-OK response or
 * network failure. The error carries an HTTP status and a machine code so the
 * caller can render a deliberate, honest state rather than a raw string.
 */
async function fetchJson<T>(url: string, init?: RequestInit): Promise<T> {
  let res: Response;
  try {
    res = await fetchWithTimeout(url, init);
  } catch {
    throw new ApiError(0, "NETWORK", "Could not reach the indexer");
  }
  if (!res.ok) {
    let code = "";
    let message = "";
    let requestId: string | undefined;
    try {
      const body = (await res.json()) as {
        error?: { code?: string; message?: string; request_id?: string };
      };
      code = body?.error?.code ?? "";
      message = body?.error?.message ?? "";
      requestId = body?.error?.request_id;
    } catch {
      // Non-JSON error body — fall through with generic values.
    }
    throw new ApiError(res.status, code || `HTTP_${res.status}`, message, requestId);
  }
  return (await res.json()) as T;
}

export async function listEvents(
  params: QueryEventsParams = {},
): Promise<ListEventsResponse> {
  const network: Network = params.network ?? "testnet";
  const url = new URL(`${baseUrl(network)}/v1/events`);
  if (params.contractId) url.searchParams.set("contractId", params.contractId);
  if (params.topic0) url.searchParams.set("topic0", params.topic0);
  if (params.ledgerFrom != null)
    url.searchParams.set("ledgerFrom", String(params.ledgerFrom));
  if (params.ledgerTo != null)
    url.searchParams.set("ledgerTo", String(params.ledgerTo));
  if (params.cursor) url.searchParams.set("cursor", params.cursor);
  url.searchParams.set("limit", String(params.limit ?? 25));

  return fetchJson<ListEventsResponse>(url.toString(), { headers: authHeaders() });
}

export async function getEvent(
  id: string,
  network: Network = "testnet",
): Promise<SorobanEvent> {
  const body = await fetchJson<{ event: SorobanEvent }>(
    `${baseUrl(network)}/v1/events/${encodeURIComponent(id)}`,
    { headers: authHeaders() },
  );
  return body.event;
}

/**
 * Build the Trident SSE stream URL for a contract. This is fetched by the
 * explorer's own /api/events/stream proxy (never directly from the browser),
 * so the API key and the Last-Event-ID handshake stay server-side.
 */
export function streamUrl(network: Network, contractId: string, topic0 = ""): string {
  const url = new URL(`${baseUrl(network)}/v1/events/stream`);
  url.searchParams.set("contractId", contractId);
  if (topic0) url.searchParams.set("topic0", topic0);
  return url.toString();
}

/** Base headers for streaming requests (server-side only). */
export function streamHeaders(lastEventId?: string): HeadersInit {
  const h: Record<string, string> = {
    Accept: "text/event-stream",
    "Cache-Control": "no-cache",
  };
  if (API_KEY) h["X-API-Key"] = API_KEY;
  if (lastEventId) h["Last-Event-ID"] = lastEventId;
  return h;
}

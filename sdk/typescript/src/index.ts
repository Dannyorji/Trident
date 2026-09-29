import { z } from "zod";
import { parseApiError, TridentApiError, TridentError } from "./errors.js";
import { redactKey, resolveApiKey, resolveApiUrl } from "./config.js";
import { createSubscription } from "./subscription.js";
import { iterEvents as iterEventsImpl } from "./iterator.js";
import type { IterEventsOptions } from "./iterator.js";
import {
  computeBackoffMs,
  isRetryableStatus,
  parseRetryAfterMs,
  resolveRetryConfig,
  sleep,
} from "./retry.js";
import type { RetryConfig } from "./retry.js";

export { TridentError, TridentApiError } from "./errors.js";
export type { TridentErrorCode } from "./errors.js";
export { ENV_API_KEY, ENV_BASE_URL, redactKey } from "./config.js";
export { iterEvents, DEFAULT_MAX_PAGES } from "./iterator.js";
export type { IterEventsOptions, QueryEventsFn } from "./iterator.js";
export type { components, operations, paths } from "./api-types.gen.js";
export { DEFAULT_RETRY_CONFIG } from "./retry.js";
export type { RetryConfig } from "./retry.js";
export {
  computeSignature,
  DEFAULT_TOLERANCE_SECONDS,
  verifySignature,
  WebhookVerificationError,
} from "./webhook.js";

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

export type Network = "mainnet" | "testnet" | "futurenet";
export type TransportType = "rest" | "graphql";

/**
 * Default per-attempt REST timeout. Without one, a stalled server holds the
 * caller open indefinitely: `fetch` has no built-in timeout, and the retry
 * layer only reacts to a settled rejection, so a hung socket is never retried.
 */
export const DEFAULT_TIMEOUT_MS = 30_000;

/**
 * Resolves the effective per-attempt timeout: an explicit per-call value wins,
 * then the client-level value, then {@link DEFAULT_TIMEOUT_MS}. `false` at
 * either level disables the timeout. Non-finite or non-positive values are
 * rejected rather than silently producing an immediate abort.
 */
function resolveTimeoutMs(
  override: number | false | undefined,
  clientValue: number | false | undefined,
): number | false {
  const chosen = override ?? clientValue ?? DEFAULT_TIMEOUT_MS;
  if (chosen === false) return false;
  if (!Number.isFinite(chosen) || chosen <= 0) {
    throw new TridentError("INTERNAL", `timeoutMs must be a positive number or false, got ${chosen}`);
  }
  return chosen;
}

export interface TridentClientConfig {
  /** Falls back to the TRIDENT_BASE_URL environment variable when omitted. */
  apiUrl?: string;
  /** Falls back to the TRIDENT_API_KEY environment variable when omitted. */
  apiKey?: string;
  network: Network;
  webSocketImpl?: any;
  transport?: TransportType;
  /**
   * Retry policy applied to idempotent (GET) REST requests. Honours
   * `Retry-After` on 429/503 responses, falling back to exponential backoff
   * with jitter otherwise. Pass `false` to disable retries for this client.
   * Defaults to {@link DEFAULT_RETRY_CONFIG}.
   */
  retry?: RetryConfig | false;
  /**
   * Per-attempt timeout in milliseconds for REST requests. Each retry attempt
   * gets a fresh budget, so this bounds how long any single request can hang
   * rather than the retry sequence as a whole. Pass `false` to wait
   * indefinitely. Defaults to {@link DEFAULT_TIMEOUT_MS}.
   */
  timeoutMs?: number | false;
}

/** Per-call options accepted by {@link TridentClient.queryEvents} and {@link TridentClient.getEventById}. */
export interface RequestOptions {
  /** Overrides the client-level `retry` config for this call only. */
  retry?: RetryConfig | false;
  /** Overrides the client-level `timeoutMs` for this call only. */
  timeoutMs?: number | false;
}

// ---------------------------------------------------------------------------
// Domain types
// ---------------------------------------------------------------------------

export const EventTypeSchema = z.enum(["contract", "system", "diagnostic"]);
export type EventType = z.infer<typeof EventTypeSchema>;

export const SorobanEventSchema = z.object({
  id: z.string(),
  contractId: z.string(),
  ledgerSequence: z.number().int().nonnegative(),
  ledgerTimestamp: z.string(),
  transactionHash: z.string(),
  eventIndex: z.number().int().nonnegative(),
  eventType: EventTypeSchema,
  topics: z.array(z.string()),
  data: z.unknown(),
  createdAt: z.string(),
});
export type SorobanEvent = z.infer<typeof SorobanEventSchema>;

// ---------------------------------------------------------------------------
// Query parameter types
// ---------------------------------------------------------------------------

export interface QueryEventsParams {
  contractId?: string;
  topic0?: string;
  topic1?: string;
  ledgerFrom?: number;
  ledgerTo?: number;
  after?: string;
  limit?: number;
  eventType?: "contract" | "system" | "diagnostic";
}

export interface GetEventByIdParams {
  id: string;
}

export interface SubscribeToContractParams {
  contractId: string;
  topic0?: string;
  onEvent: (event: SorobanEvent) => void;
  onError?: (error: Error) => void;
}

export interface Subscription {
  unsubscribe: () => void;
}

export interface PaginatedEvents {
  events: SorobanEvent[];
  cursor: string | null;
  hasMore: boolean;
}

// ---------------------------------------------------------------------------
// Internal API response schemas (snake_case, as returned by the Go API)
// ---------------------------------------------------------------------------

const ApiEventSchema = z.object({
  id: z.string(),
  contract_id: z.string(),
  ledger_sequence: z.number().int().nonnegative(),
  ledger_timestamp: z.string(),
  transaction_hash: z.string(),
  event_index: z.number().int().nonnegative(),
  event_type: z.string(),
  topics: z.array(z.string()),
  data: z.string(),
  created_at: z.string(),
});

const ApiListEventsResponseSchema = z.object({
  events: z.array(ApiEventSchema),
  next_cursor: z.string().nullable(),
  has_more: z.boolean(),
});

function apiEventToSorobanEvent(
  e: z.infer<typeof ApiEventSchema>,
): SorobanEvent {
  return SorobanEventSchema.parse({
    id: e.id,
    contractId: e.contract_id,
    ledgerSequence: e.ledger_sequence,
    ledgerTimestamp: e.ledger_timestamp,
    transactionHash: e.transaction_hash,
    eventIndex: e.event_index,
    eventType: e.event_type,
    topics: e.topics,
    data: (() => {
      try {
        return JSON.parse(e.data);
      } catch {
        return e.data;
      }
    })(),
    createdAt: e.created_at,
  });
}

// ---------------------------------------------------------------------------
// Client
// ---------------------------------------------------------------------------

export class TridentClient {
  private readonly config: TridentClientConfig;
  private readonly apiUrl: string;
  private readonly apiKey: string;
  private readonly transport: "rest" | "graphql";
  private graphqlTransport?: any; // Lazy-loaded GraphQL transport

  constructor(config: TridentClientConfig) {
    this.config = config;
    this.apiUrl = resolveApiUrl(config.apiUrl);
    this.apiKey = resolveApiKey(config.apiKey);
    this.transport = config.transport ?? "rest";
  }

  /** Redacted string representation — never includes the raw API key. */
  toString(): string {
    return `TridentClient(apiUrl=${this.apiUrl}, apiKey=${redactKey(this.apiKey)})`;
  }

  /** Ensures Node's `console.log`/`util.inspect` also redact the API key. */
  [Symbol.for("nodejs.util.inspect.custom")](): string {
    return this.toString();
  }

  private get headers(): Record<string, string> {
    return {
      "X-API-Key": this.apiKey,
      "Content-Type": "application/json",
    };
  }

  private async fetchJSON<T>(
    url: string,
    schema: z.ZodType<T>,
    retryOverride?: RetryConfig | false,
    timeoutOverride?: number | false,
  ): Promise<T> {
    const retryCfg = resolveRetryConfig(retryOverride, this.config.retry);
    const maxAttempts = retryCfg ? retryCfg.maxAttempts : 1;
    const timeoutMs = resolveTimeoutMs(timeoutOverride, this.config.timeoutMs);
    let totalWaitedMs = 0;

    for (let attempt = 1; ; attempt++) {
      let res: Response;
      // A fresh controller per attempt: an aborted signal stays aborted, so
      // reusing one would make every retry fail instantly.
      const controller = timeoutMs === false ? undefined : new AbortController();
      const timer =
        controller === undefined
          ? undefined
          : setTimeout(() => controller.abort(), timeoutMs as number);
      try {
        res = await fetch(url, { headers: this.headers, signal: controller?.signal });
      } catch (cause) {
        const timedOut = controller?.signal.aborted === true;
        if (retryCfg && attempt < maxAttempts) {
          const waitMs = computeBackoffMs(attempt, retryCfg);
          if (totalWaitedMs + waitMs <= retryCfg.maxTotalWaitMs) {
            totalWaitedMs += waitMs;
            await sleep(waitMs);
            continue;
          }
        }
        const err = timedOut
          ? new TridentError(
              "TIMEOUT",
              `Request timed out after ${timeoutMs}ms${attempt > 1 ? ` on each of ${attempt} attempts` : ""}`,
              cause,
            )
          : new TridentError(
              attempt > 1 ? "RETRY_EXHAUSTED" : "INTERNAL",
              attempt > 1
                ? `Network request failed after ${attempt} attempts`
                : "Network request failed",
              cause,
            );
        err.attempts = attempt;
        throw err;
      } finally {
        if (timer !== undefined) clearTimeout(timer);
      }

      if (!res.ok) {
        if (retryCfg && isRetryableStatus(res.status) && attempt < maxAttempts) {
          const retryAfterMs = parseRetryAfterMs(res.headers?.get?.("retry-after"));
          const waitMs = retryAfterMs ?? computeBackoffMs(attempt, retryCfg);
          if (totalWaitedMs + waitMs <= retryCfg.maxTotalWaitMs) {
            totalWaitedMs += waitMs;
            await sleep(waitMs);
            continue;
          }
        }
        const body = await res.text().catch(() => "");
        const apiError = parseApiError(res.status, body);
        apiError.attempts = attempt;
        throw apiError;
      }

      const json: unknown = await res.json().catch((cause: unknown) => {
        throw new TridentError("INTERNAL", "Failed to parse response JSON", cause);
      });

      return schema.parse(json);
    }
  }

  private async getGraphQLTransport() {
    if (this.graphqlTransport) {
      return this.graphqlTransport;
    }
    // Lazy load GraphQL transport only when needed
    const { GraphQLTransport } = await import("./transports/graphql.js");
    this.graphqlTransport = new GraphQLTransport(this.apiUrl, this.apiKey);
    return this.graphqlTransport;
  }

  /**
   * Query historical Soroban events with optional filtering.
   *
   * Results are cursor-paginated — pass the returned `cursor` as `after` on
   * the next call to fetch the next page.
   */
  async queryEvents(
    params: QueryEventsParams,
    options?: RequestOptions,
  ): Promise<PaginatedEvents> {
    if (this.transport === "graphql") {
      const transport = await this.getGraphQLTransport();
      return transport.queryEvents(
        params.contractId,
        params.topic0,
        params.topic1,
        params.ledgerFrom,
        params.ledgerTo,
        params.limit,
        params.after,
      );
    }

    // REST transport (default)
    const qs = new URLSearchParams();
    if (params.contractId) qs.set("contractId", params.contractId);
    if (params.topic0) qs.set("topic0", params.topic0);
    if (params.topic1) qs.set("topic1", params.topic1);
    if (params.ledgerFrom !== undefined)
      qs.set("ledgerFrom", String(params.ledgerFrom));
    if (params.ledgerTo !== undefined)
      qs.set("ledgerTo", String(params.ledgerTo));
    if (params.after) qs.set("cursor", params.after);
    if (params.limit !== undefined) qs.set("limit", String(params.limit));
    if (params.eventType) qs.set("event_type", params.eventType);

    const url = `${this.config.apiUrl}/v1/events?${qs.toString()}`;
    const resp = await this.fetchJSON(
      url,
      ApiListEventsResponseSchema,
      options?.retry,
      options?.timeoutMs,
    );

    return {
      events: resp.events.map(apiEventToSorobanEvent),
      cursor: resp.next_cursor,
      hasMore: resp.has_more,
    };
  }

  /**
   * Auto-paginating async iterator over {@link queryEvents}.
   *
   * Yields every matching event across every page, following the server's
   * cursor automatically until there are no more results — so callers can just
   * write `for await (const event of client.iterEvents(params))` instead of
   * hand-rolling a `hasMore`/`cursor` loop.
   *
   * Stops when the server reports `has_more === false`. Fetches at most
   * `options.maxPages` pages (default 100); if that limit is reached while more
   * results remain, throws `TridentError` with code `ITERATION_LIMIT`. Any
   * `TridentError` from an underlying page request propagates transparently.
   */
  iterEvents(
    params: QueryEventsParams,
    options?: IterEventsOptions,
  ): AsyncIterable<SorobanEvent> {
    return iterEventsImpl(
      (p: QueryEventsParams) => this.queryEvents(p),
      params,
      options,
    );
  }

  /**
   * Fetch a single event by its UUID.
   *
   * Throws `TridentError` with code `NOT_FOUND` if no event exists.
   */
  async getEventById(
    params: GetEventByIdParams,
    options?: RequestOptions,
  ): Promise<SorobanEvent> {
    if (this.transport === "graphql") {
      const transport = await this.getGraphQLTransport();
      return transport.getEventById(params.id);
    }

    // REST transport (default)
    const url = `${this.config.apiUrl}/v1/events/${encodeURIComponent(params.id)}`;
    const apiEvent = await this.fetchJSON(url, ApiEventSchema, options?.retry, options?.timeoutMs);
    return apiEventToSorobanEvent(apiEvent);
  }

  /**
   * Open a real-time WebSocket subscription to events emitted by a contract.
   *
   * For GraphQL transport, requires graphql-ws to be installed.
   * For REST transport, uses native WebSocket.
   */
  subscribeToContract(params: SubscribeToContractParams): Subscription {
    if (params.topic0 !== undefined && params.topic0 === "") {
      throw new TridentApiError(
        400,
        "INVALID_ARGUMENT",
        "topic0 must not be an empty string; omit the field to receive all events",
      );
    }

    if (this.transport === "graphql") {
      // GraphQL subscriptions require graphql-ws
      try {
        // Attempt to import graphql-ws
        require("graphql-ws");
      } catch {
        throw new TridentError(
          "INTERNAL",
          "GraphQL subscriptions require graphql-ws. Install it with: npm install graphql-ws",
        );
      }

      // Use graphql-ws protocol for subscriptions
      // This will be implemented via the graphql-ws client library
      throw new TridentError(
        "INTERNAL",
        "GraphQL subscriptions are not yet fully implemented",
      );
    }

    // REST transport (default) - use native WebSocket
    const wsBase = this.apiUrl
      .replace(/^https:\/\//, "wss://")
      .replace(/^http:\/\//, "ws://");

    const qs = new URLSearchParams({ contractId: params.contractId });
    if (params.topic0) qs.set("topic0", params.topic0);

    const wsUrl = `${wsBase}/ws?${qs.toString()}`;
    return createSubscription(wsUrl, params, this.config.webSocketImpl);
  }
}

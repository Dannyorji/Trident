export interface SorobanEvent {
  id: string;
  contract_id: string;
  ledger_sequence: number;
  ledger_timestamp: string;
  transaction_hash: string;
  event_index: number;
  event_type: string;
  topics: string[];
  data: string;
  created_at: string;
}

export interface ListEventsResponse {
  events: SorobanEvent[];
  has_more: boolean;
  next_cursor: string | null;
}

export type Network = 'testnet' | 'mainnet';

/**
 * The deliberate states the explorer can be in for a contract. Each maps to a
 * distinct, honest panel that explains what happened and what to do next.
 */
export type ExplorerState =
  | 'loading'
  | 'ok'
  | 'no_events'
  | 'not_indexed'
  | 'invalid_contract'
  | 'api_unreachable'
  | 'not_found';

export type UnreachableReason = 'network' | 'down' | 'rate_limited' | 'unauthorized' | 'timeout';

/**
 * Response envelope returned by the explorer's own /api/events.json route.
 * Extends the Trident ListEventsResponse with a classification the client can
 * render without reaching into raw error strings.
 */
export interface ExplorerEventsResponse extends ListEventsResponse {
  status: ExplorerState;
  /**
   * Reason for an `api_unreachable` status. Present only when status is
   * `api_unreachable`.
   */
  reason?: UnreachableReason;
  /**
   * True when the current request carries a topic0 / ledger-range filter. The
   * "no events" panel should then point at the filter, not the contract.
   */
  filtered?: boolean;
  /** Human-safe message for the current state (never a raw error/stack). */
  message?: string;
}

/** A single event as delivered by the SSE stream (raw Redis field casing). */
export interface StreamedEvent {
  contract_id: string;
  ledger_sequence: string;
  ledger_timestamp: string;
  transaction_hash: string;
  event_index: string;
  event_type: string;
  topics: string;
  data: string;
  event_id?: string;
}
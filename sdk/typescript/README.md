# @trident-indexer/sdk

TypeScript client SDK for the [Trident](https://github.com/Telocel-Labs/Trident) Soroban event indexer.

Query historical Soroban contract events and subscribe to real-time updates without running your own infrastructure.

## Installation

```bash
npm install @trident-indexer/sdk
# or
yarn add @trident-indexer/sdk
# or
pnpm add @trident-indexer/sdk
```

The package ships pre-built CJS + ESM bundles and a `dist/index.d.ts` declaration file for full autocomplete out of the box.

---

## Quick Start

```typescript
import { TridentClient } from "@trident-indexer/sdk";

const client = new TridentClient({
  apiUrl: "https://api.trident.telocel.io",
  apiKey: "your-api-key",
  network: "mainnet",
});

// 1. Query historical events (cursor-paginated)
const page1 = await client.queryEvents({
  contractId: "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAD2KM",
  topic0: "transfer",
  ledgerFrom: 50000,
  ledgerTo: 51000,
  limit: 50,
});

console.log(`Found ${page1.events.length} events`);

// Fetch the next page using the cursor
if (page1.hasMore && page1.cursor) {
  const page2 = await client.queryEvents({ after: page1.cursor, limit: 50 });
  console.log("Page 2:", page2.events);
}

// Or drain every page automatically with the async iterator — no manual
// cursor/hasMore bookkeeping. Stops when the server has no more results.
for await (const event of client.iterEvents({
  contractId: "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAD2KM",
  topic0: "transfer",
})) {
  console.log(event.id, event.transactionHash);
}

// A safety valve caps the number of pages fetched (default 100). If the limit
// is hit while more results remain, a TridentError(code: "ITERATION_LIMIT")
// is thrown; raise it for very large backfills:
for await (const event of client.iterEvents(
  { contractId: "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAD2KM" },
  { maxPages: 1000 },
)) {
  // ...process event
}

// 2. Fetch a single event by UUID
const event = await client.getEventById({
  id: "550e8400-e29b-41d4-a716-446655440000",
});
console.log("Event ledger:", event.ledgerSequence);

// 3. Subscribe to live events (auto-reconnects on disconnect)
const sub = client.subscribeToContract({
  contractId: "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAD2KM",
  topic0: "transfer",
  onEvent: (event) => {
    console.log("New event:", event.transactionHash, event.topics);
  },
  onError: (err) => {
    console.error("WebSocket error:", err.message);
  },
});

// Later, when done:
sub.unsubscribe();
```

---

## Configuration

Pass a `TridentClientConfig` object to the `TridentClient` constructor.

| Field     | Type                                    | Required | Description                                                                                     |
|-----------|-----------------------------------------|----------|-------------------------------------------------------------------------------------------------|
| `apiUrl`  | `string`                                | See below | Base URL of the Trident REST API (e.g. `https://api.trident.telocel.io`). No trailing slash.   |
| `apiKey`  | `string`                                | See below | API key sent as the `X-API-Key` header on every request.                                        |
| `network` | `"mainnet" \| "testnet" \| "futurenet"` | ✅       | Stellar network to query. Included in WebSocket subscription frames for server-side routing.    |

`apiUrl` and `apiKey` may be omitted from the config object; an explicit
value always takes precedence, but the SDK falls back to the
`TRIDENT_BASE_URL` / `TRIDENT_API_KEY` environment variables (Node.js only —
`process.env` is not read in browser bundles). If neither source provides a
value, the constructor throws a `TridentError` with `code: "CONFIG"`. The
API key is never logged: `client.toString()` and Node's `util.inspect`
output redact it (e.g. `***a1b2`).

---

## API Reference

### `client.queryEvents(params)`

Query historical Soroban events with optional filtering. Results are cursor-paginated.

**Signature**

```typescript
queryEvents(params: QueryEventsParams): Promise<PaginatedEvents>
```

**Parameters**

| Parameter    | Type     | Required | Description                                                                 |
|--------------|----------|----------|-----------------------------------------------------------------------------|
| `contractId` | `string` |          | Filter by Stellar contract address (C… strkey, 56 chars).                  |
| `topic0`     | `string` |          | Filter by the first event topic (e.g. `"transfer"`, `"mint"`).             |
| `topic1`     | `string` |          | Filter by the second event topic.                                           |
| `ledgerFrom` | `number` |          | Only return events from this ledger sequence onward (inclusive).            |
| `ledgerTo`   | `number` |          | Only return events up to and including this ledger sequence.                |
| `after`      | `string` |          | Pagination cursor from a previous response's `cursor` field.               |
| `limit`      | `number` |          | Maximum events per page. Must be 1–200. Server default: 50.                 |

**Returns:** `Promise<PaginatedEvents>`

**Throws:** `TridentError` on network failure, auth failure, or server error.

---

### `client.getEventById(params)`

Fetch a single Soroban event by its UUID.

**Signature**

```typescript
getEventById(params: GetEventByIdParams): Promise<SorobanEvent>
```

**Parameters**

| Parameter | Type     | Required | Description                        |
|-----------|----------|----------|------------------------------------|
| `id`      | `string` | ✅       | UUID v4 of the event to retrieve.  |

**Returns:** `Promise<SorobanEvent>`

**Throws:** `TridentError` with `code: "NOT_FOUND"` if the event does not exist.

---

### `client.subscribeToContract(params)`

Open a real-time WebSocket subscription to events from a contract.

The SDK reconnects automatically with exponential backoff (500ms initial, 30s maximum) on unexpected disconnects. Calling `unsubscribe()` cancels any pending reconnect.

**Signature**

```typescript
subscribeToContract(params: SubscribeToContractParams): Subscription
```

**Parameters**

| Parameter    | Type                            | Required | Description                                                               |
|--------------|---------------------------------|----------|---------------------------------------------------------------------------|
| `contractId` | `string`                        | ✅       | Stellar contract address to subscribe to.                                 |
| `topic0`     | `string`                        |          | Optional topic filter applied server-side.                                |
| `onEvent`    | `(event: SorobanEvent) => void` | ✅       | Called for every incoming event matching the filter.                      |
| `onError`    | `(error: Error) => void`        |          | Called on WebSocket errors; the subscription continues to auto-reconnect. |

**Returns:** `Subscription` — call `.unsubscribe()` to close the connection.

---

## Error Handling

All methods throw `TridentError` on failure. Branch on `error.code` to handle specific cases:

```typescript
import { TridentClient, TridentError } from "@trident-indexer/sdk";

const client = new TridentClient({ apiUrl: "...", apiKey: "...", network: "testnet" });

try {
  const event = await client.getEventById({ id: "550e8400-e29b-41d4-a716-446655440000" });
} catch (err) {
  if (err instanceof TridentError) {
    switch (err.code) {
      case "NOT_FOUND":
        console.error("Event not found");
        break;
      case "UNAUTHORIZED":
        console.error("Invalid or missing API key");
        break;
      case "RATE_LIMITED":
        console.error("Too many requests — back off and retry");
        break;
      case "INTERNAL":
      default:
        console.error("Unexpected error:", err.message);
    }
  } else {
    throw err;
  }
}
```

### Error codes

| Code           | HTTP status | When                                                    |
|----------------|-------------|---------------------------------------------------------|
| `NOT_FOUND`    | 404         | The requested event or resource does not exist.         |
| `UNAUTHORIZED` | 401         | The `X-API-Key` header is missing or invalid.           |
| `RATE_LIMITED` | 429         | Too many requests in a short window.                    |
| `INTERNAL`     | 5xx / other | Unexpected server error or network failure.             |

---

## TypeScript Types

The SDK exports all types needed to work with indexed Soroban events.

```typescript
import type {
  SorobanEvent,
  EventType,
  PaginatedEvents,
  Subscription,
  TridentClientConfig,
  QueryEventsParams,
  GetEventByIdParams,
  SubscribeToContractParams,
  TridentErrorCode,
} from "@trident-indexer/sdk";
```

The client also exposes `iterEvents(params, options?)`, an auto-paginating
`AsyncIterable<SorobanEvent>` that follows the server cursor until
`hasMore === false`. It fetches at most `options.maxPages` pages (default `100`),
throwing `TridentError(code: "ITERATION_LIMIT")` if exceeded; `TridentError`s
from page requests propagate out of the `for await` loop unchanged. Also
exported standalone as `iterEvents(queryEvents, params, options?)`.

### `SorobanEvent`

A single indexed Soroban contract event.

```typescript
interface SorobanEvent {
  /** UUID v4 assigned by the indexer. */
  id: string;
  /** Stellar contract address that emitted the event (C… strkey). */
  contractId: string;
  /** Ledger sequence number in which the event occurred. */
  ledgerSequence: number;
  /** ISO-8601 timestamp of the ledger close. */
  ledgerTimestamp: string;
  /** Transaction hash (hex). */
  transactionHash: string;
  /** Zero-based index of this event within the transaction. */
  eventIndex: number;
  /** Whether this is a contract-emitted, system, or diagnostic event. */
  eventType: EventType;
  /** Array of XDR-encoded topic values (as base64 strings). */
  topics: string[];
  /** Decoded event data payload. */
  data: unknown;
  /** ISO-8601 timestamp when this record was written to the index. */
  createdAt: string;
}

type EventType = "contract" | "system" | "diagnostic";
```

### `PaginatedEvents`

Returned by `queryEvents`.

```typescript
interface PaginatedEvents {
  /** Events matching the query on this page. */
  events: SorobanEvent[];
  /** Pass as `after` on the next call to fetch the next page. Null when no more pages exist. */
  cursor: string | null;
  /** True when more pages are available. */
  hasMore: boolean;
}
```

### `Subscription`

Returned by `subscribeToContract`.

```typescript
interface Subscription {
  /** Close the WebSocket connection and cancel any pending reconnect. */
  unsubscribe: () => void;
}
```

---

## Node.js Compatibility

The SDK works seamlessly in both browser and Node.js environments.

- **Browsers & Node.js 21+:** Uses the native global `WebSocket` (no extra configuration required).
- **Node.js 18 & 20:** The SDK will automatically and dynamically import the `ws` library at runtime if the global `WebSocket` is not found. Ensure `ws` is installed in your project:
  ```bash
  npm install ws
  ```

### Custom WebSocket Implementation

If you want to provide a specific custom WebSocket implementation (e.g., for unit testing or to force a specific client), you can pass it to the `TridentClient` constructor via the `webSocketImpl` option:

```typescript
import { TridentClient } from "@trident-indexer/sdk";
import CustomWebSocket from "ws";

const client = new TridentClient({
  apiUrl: "https://api.trident.telocel.io",
  apiKey: "your-api-key",
  network: "mainnet",
  webSocketImpl: CustomWebSocket,
});
```

---

## Development

```bash
# Install dependencies
npm install

# Build CJS + ESM bundles and TypeScript declarations
npm run build

# Run unit tests
npm test

# Type-check without emitting files
npm run lint

# Watch mode — rebuild on every file change
npm run dev
```

Build output lives in `dist/`:

```
dist/
  index.js      # CommonJS bundle (require)
  index.mjs     # ESM bundle (import)
  index.d.ts    # TypeScript declarations
```

---

## Contributing

1. Fork the repo and create a branch from `main`.
2. Add or update tests in `tests/` for any changed behaviour.
3. Run `npm test` and `npm run lint` — both must pass before opening a PR.
4. Open a pull request against `Telocel-Labs/Trident` and link any relevant issues.

---

## License

MIT

## Regenerating OpenAPI models

See [docs/sdk-regeneration.md](../../docs/sdk-regeneration.md) for the full cross-SDK procedure (regenerating all SDKs together, version consistency, testing after regeneration). Quick version — install the generator dependency once with `python3 -m pip install PyYAML`, then run:

```bash
python3 scripts/generate_sdk_models.py --language typescript
```

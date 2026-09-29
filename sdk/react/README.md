# @trident-indexer/react

React hooks for the [Trident](https://github.com/Telocel-Labs/Trident) Soroban event indexer, built on top of [`@trident-indexer/sdk`](../typescript).

## Installation

```bash
npm install @trident-indexer/react @trident-indexer/sdk
# or
yarn add @trident-indexer/react @trident-indexer/sdk
# or
pnpm add @trident-indexer/react @trident-indexer/sdk
```

`@trident-indexer/sdk` is a peer dependency, not bundled — the TypeScript
SDK's `TridentClient` and its types are what `TridentProvider` wraps and
every hook re-exports for convenience.

---

## Quick Start

Wrap your app (or the portion of it that needs event data) in
`TridentProvider`:

```tsx
import { TridentProvider } from "@trident-indexer/react";

function App() {
  return (
    <TridentProvider apiUrl="https://api.trident.telocel.io" apiKey="your-api-key" network="mainnet">
      <EventFeed />
    </TridentProvider>
  );
}
```

Then use the hooks anywhere beneath the provider:

```tsx
import { useContractEvents, useSubscription } from "@trident-indexer/react";

function EventFeed() {
  const { events, isLoading, error, hasMore, refresh } = useContractEvents({
    contractId: "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAD2KM",
    topic0: "transfer",
    limit: 50,
  });

  const { lastEvent, isConnected } = useSubscription({
    contractId: "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAD2KM",
    topic0: "transfer",
    onEvent: (event) => console.log("new event:", event.transactionHash),
  });

  if (isLoading) return <p>Loading…</p>;
  if (error) return <p>Error: {error.message}</p>;

  return (
    <div>
      <p>Live: {isConnected ? "connected" : "disconnected"}</p>
      {lastEvent && <p>Latest: {lastEvent.transactionHash}</p>}
      <ul>
        {events.map((e) => (
          <li key={e.id}>{e.transactionHash}</li>
        ))}
      </ul>
      {hasMore && <button onClick={refresh}>Refresh</button>}
    </div>
  );
}
```

---

## API

### `TridentProvider`

| Prop | Type | Default | Description |
|---|---|---|---|
| `apiUrl` | `string` | `TRIDENT_BASE_URL` env var (SSR only) | Trident API base URL. |
| `apiKey` | `string` | `TRIDENT_API_KEY` env var (SSR only) | API key. |
| `network` | `TridentClientConfig["network"]` | `"mainnet"` | Network the client targets. |

Constructs one `TridentClient` (stable across re-renders unless `apiUrl`,
`apiKey`, or `network` change) and makes it available to every hook beneath
it via context.

### `useContractEvents(params)`

Cursor-paginated historical event query. `params` extends the TypeScript
SDK's `QueryEventsParams` (`contractId`, `topic0`, `ledgerFrom`,
`ledgerTo`, `limit`, …) plus an optional `refreshInterval` (milliseconds;
no auto-refresh by default).

Returns `{ events, cursor, hasMore, isLoading, error, refresh }` —
`refresh()` triggers a manual re-fetch with the same params.

### `useSubscription(params)`

Real-time subscription over the SDK's WebSocket transport. `params`:
`contractId`, an optional `topic0`, and optional `onEvent`/`onError`
callbacks.

Returns `{ lastEvent, isConnected }` — `lastEvent` is `null` until the first
event arrives; `onEvent` fires on every event in addition to `lastEvent`
updating, for callers that need side effects rather than a render-driven
value.

### Re-exported types

`SorobanEvent`, `QueryEventsParams`, and `TridentClientConfig` are
re-exported from `@trident-indexer/sdk` so most consumers never need a
direct import from the underlying package.

---

## Regenerating OpenAPI models

This package has no generated model file of its own — `useContractEvents`
and `useSubscription` consume `@trident-indexer/sdk`'s `SorobanEvent` and
`QueryEventsParams` types directly rather than generating a parallel copy.
Regenerating the TypeScript SDK's models
(`python3 scripts/generate_sdk_models.py --language typescript`, see
[`sdk/typescript/README.md`](../typescript/README.md#regenerating-openapi-models))
is what keeps this package's types current — there is no separate step to
run here.

---

## Development

```bash
npm install
npm run build   # requires @trident-indexer/sdk (../typescript) to be built first —
                 # its dist/ output is what this package's file: dependency resolves to
npm test
```

## License

MIT

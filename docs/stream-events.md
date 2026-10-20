# StreamEvents

The gRPC `StreamEvents` RPC is the internal transport for live events: the Rust
gRPC server consumes the Redis stream the indexer writes and pushes typed
`Event` messages to one subscriber (issue #236). It is not exposed to clients
directly — the Go layer fans it out over SSE and WebSocket.

## Request

| Field         | Meaning                                                       |
| ------------- | ------------------------------------------------------------- |
| `contract_id` | Required. Only events from this contract are pushed.           |
| `topic_0`     | Optional. Only events whose first topic matches are pushed.    |
| `start_id`    | Optional resume point. Empty means live tail.                  |

## Network and event id

Every stream entry carries `network` and the event UUID (`id`, also written as
`event_id`), so a streamed event's `id` equals the id returned by
`GET /v1/events/{id}`. Send the subscriber key's network in the
`x-trident-network` request metadata; entries from any other network are never
pushed. SSE applies the authenticated key's network the same way, and sends a
`: ping` comment every 15 seconds on an idle stream so proxies do not close it.

## Resuming

`start_id` is a Redis stream entry ID — `<millis>-<seq>` — normally the last ID
the client received. On reconnect the server replays from there instead of
dropping everything published while the client was away, which mirrors the SSE
`Last-Event-ID` contract.

Two special values are accepted: `$` (live tail, the default) and `0` (replay
everything still retained in the stream). Anything else is rejected with
`InvalidArgument`; passing a malformed ID through to Redis would surface as an
opaque connection error to the subscriber instead of a clear one.

Replay depth is bounded by `REDIS_STREAM_MAXLEN` on the indexer side. A client
offline longer than that window resumes from the oldest retained entry, so the
stream is not a durable log — historical gaps are filled through `ListEvents`.

## Cancellation

When a client disconnects, tonic drops the stream, which drops the receiving
half of the channel. The consumer races its blocking `XREAD` against the channel
closing, so it returns immediately rather than staying parked for the remainder
of the 5-second block window. No task outlives its subscriber.

## Backpressure

Each subscriber has a bounded buffer, `STREAM_CHANNEL_BUFFER` (default 128).
When it fills, the consumer blocks on send and stops reading Redis for that
subscriber. That is the intended behaviour: a slow client throttles its own
stream instead of making the server queue events without limit. Other
subscribers are unaffected — each has its own consumer and buffer.

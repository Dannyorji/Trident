# Structured Logging & Correlation IDs (issue #294)

Both Trident stacks — the Go REST API (`services/api`) and the Rust services
(`crates/indexer`, `crates/api`) — emit **one JSON object per log line**, and
share the concepts below (a `service` field, a per-request correlation id,
one line per request). The two are not byte-identical, though: Go's `slog`
uses its own default key names and casing (`time`/`msg`, `level` uppercased
as `INFO`/`DEBUG`/etc.) rather than the `timestamp`/`message`/lowercase
convention below, which describes the Rust side. A log aggregator ingesting
both needs a field-mapping rule per stack, not one shared parser (issue
#239 standardised the *Go* side's own fields; unifying the two stacks onto
one literal wire format is a separate, not-yet-scheduled piece of work).

## Schema

| Field | Type | Always present | Description |
|-------|------|:--:|-------------|
| `service` | string | ✅ | Emitting service: `trident-api` (Go REST), `trident-grpc-api` (Rust gRPC), `trident-indexer` (Rust indexer) |
| `level` | string | ✅ | Severity. Rust: lowercase (`debug`\|`info`\|`warn`\|`error`). Go: `slog`'s default uppercase (`DEBUG`\|`INFO`\|`WARN`\|`ERROR`) |
| `time` | string | ✅ | RFC 3339 UTC, Go's default `slog` key (e.g. `2024-01-01T12:00:00Z`) |
| `msg` | string | ✅ | Human-readable log message, Go's default `slog` key |
| `request_id` | string | Go: request-scoped | Per-request correlation id (see below) |
| `route` | string | Go: on the per-request summary line | Registered `ServeMux` pattern (e.g. `GET /v1/events/{id}`), not the raw path — path parameters never blow up cardinality (issue #239) |
| `api_key_id` | string | Go: once authenticated | The authenticated API key's UUID. Absent (not empty) on a request rejected before authenticating — a 401, or one dropped by rate limiting (issue #239) |
| `target` | string | Rust only | Rust module path that emitted the event |
| _\<structured fields\>_ | any | — | Additional key/value context (e.g. `status`, `latency_ms`, `method`) |

Example (Go REST API per-request summary line, emitted by `middleware.StructuredLogging`):

```json
{"time":"2024-01-01T12:00:00Z","level":"INFO","msg":"http_request","service":"trident-api","request_id":"a1b2c3d4e5f6a7b8","method":"GET","route":"GET /v1/events/{id}","status":200,"latency_ms":4,"api_key_id":"3fa85f64-5717-4562-b3fc-2c963f66afa6"}
```

`api_key_id` is a UUID, never the raw `X-API-Key` value — the API never logs a
usable credential, only the opaque row id (verified by
`TestStructuredLogging_NeverLogsTheRawAPIKey`,
`services/api/middleware/structured_logging_test.go`).

Example (Rust indexer line within a request-scoped span):

```json
{"service":"trident-indexer","level":"info","timestamp":"2024-01-01T12:00:00Z","target":"trident_indexer::streamer","message":"handling request","request_id":"a1b2c3d4e5f6a7b8","trace_id":"0af7651916cd43dd8448eb211c80319c"}
```

## Correlation IDs

### `request_id`

- **Go API** (`services/api/middleware`): the `RequestID` middleware reuses an
  inbound `X-Request-ID` header if present, otherwise generates a random
  UUID. It is stored on the request context (`httputil.RequestIDFromContext`)
  and echoed back in the `X-Request-ID` response header. `StructuredLogging`
  reads it onto the one `http_request` summary line it emits per request.
  This is the request-id mechanism from #226.
  **Not yet propagated onto every individual log line a handler emits** — a
  `slog.ErrorContext` call inside a handler logs whatever attributes that
  call site passes explicitly; it does not automatically inherit
  `request_id`/`route`/`api_key_id` from the summary line. Making every
  call site request-scoped (either a context-aware `slog.Handler` or
  converting each remaining `slog.Error`/`slog.Debug` call to pass the id
  explicitly) is a larger, separate pass across every handler, called out as
  follow-up in `services/api/logging.go`'s `initLogger` doc comment.
- **Rust**: any log emitted inside a span carrying a `request_id` field inherits
  it automatically (the shared JSON layer merges span fields onto every event).

### `trace_id`

- **Go API**: `TracingMiddleware` (`services/api/middleware/tracing.go`)
  extracts the inbound **W3C `traceparent`** header and starts a real
  OpenTelemetry span carrying that trace id, so distributed traces already
  correlate correctly. That trace id is **not currently copied onto
  structured log lines** — there is no `trace_id` slog attribute anywhere in
  `services/api`. Surfacing it (reading
  `trace.SpanContextFromContext(ctx).TraceID()` into a log attribute) would
  let a trace and its logs be cross-referenced directly; it is not part of
  what issue #239 scoped, so it is a candidate follow-up rather than
  something this doc can claim is already true.
- **Rust**: any log emitted inside a span carrying a `trace_id` field
  inherits it automatically (the shared JSON layer merges span fields onto
  every event) — see `trident_common::logging` for that side.

## Implementation

- **Go** (issue #239): `initLogger` (`services/api/logging.go`) installs a
  process-wide JSON (production) or text (otherwise) `slog.Handler` at
  startup, with `service: "trident-api"` pinned as a base attribute so it
  appears on every line without each call site adding it. `RequestID`
  attaches a request id to the context and echoes it on `X-Request-ID`.
  `StructuredLogging(mux)` — mux is used to resolve the registered route
  pattern via `mux.Handler(r)`, the same technique `NewMetrics` uses for
  metrics labels, so path parameters never blow up cardinality — wraps
  everything and, once the wrapped chain returns, emits one `http_request`
  line per request: `request_id`, `method`, `route`, `status`,
  `latency_ms`, and `api_key_id` when the request authenticated. The
  middleware chain is `RequestID(StructuredLogging(...))`, deliberately
  outermost, so a request rejected before authenticating (a 401, a 429) is
  still logged. `api_key_id` reaches that line via
  `middleware.SetLogAPIKeyID`, called from `NewDBAuth` once it authenticates
  — a plain `context.WithValue` cannot carry it back up to
  `StructuredLogging`'s own request/context reference (see the
  `requestLogState` doc comment in `services/api/middleware/context.go` for
  why). High-volume debug logs can be thinned with
  `internal/logsampling.Sampler` (issue #239); wired into
  `services/api/ws/hub.go`'s per-connection register/unregister logs.
- **Rust**: `trident_common::logging::init(service)` installs a custom
  `tracing` layer (`trident_common::logging::JsonLayer`) that serialises each
  event to the schema above and merges span fields (root→leaf) onto every line,
  so request/trace ids on an enclosing span appear on all nested logs. Both
  `crates/indexer` and `crates/api` call it at startup.

## Tests

- Go: `services/api/middleware/structured_logging_test.go` asserts the
  `route`/`api_key_id`/`latency_ms` fields, that `api_key_id` is absent (not
  empty) for an unauthenticated request, and that the raw `X-API-Key` value
  never reaches the log output. `services/api/middleware/requestid_test.go`
  covers `request_id` end-to-end (header in → log line, response header, and
  error envelope). `services/api/internal/logsampling` has its own unit
  tests for the sampling rate and concurrency safety.
- Rust: `crates/common/src/logging.rs` tests assert the schema fields are
  present and that a log emitted inside a span carrying `request_id`/`trace_id`
  includes both on the line.

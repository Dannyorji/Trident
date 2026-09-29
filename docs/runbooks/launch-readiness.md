# Launch Readiness Gate

This runbook is the launch gate for the public Trident testnet experience. It joins the checks for explorer accessibility, real-data performance, staging smoke coverage, and webhook delivery guarantees into one repeatable release ritual.

## Required staging inputs

Set these repository variables before running the launch-readiness workflow:

| Variable | Purpose |
| --- | --- |
| `STAGING_URL` | Base API URL, for example `https://api-staging.trident.telocel.com`. |
| `STAGING_EXPLORER_URL` | Public explorer URL to audit from a user entry point. |
| `STAGING_CONTRACT_ID` | Busy testnet contract used for explorer/event checks. |
| `LAUNCH_MAX_HTML_BYTES` | Optional page-weight budget, defaults to `350000`. |
| `LAUNCH_MAX_RESPONSE_MS` | Optional response-time budget, defaults to `2500`. |

Set `STAGING_API_KEY` as a repository secret for authenticated API and stream checks.

## Accessibility and responsive pass

Before launch, check the landing page, explorer, status page, and contract detail page at 320px, 768px, 1024px, and desktop widths. The launch-readiness workflow enforces a basic HTML accessibility smoke check by requiring a document title, a language attribute, and a main landmark on the explorer shell. Manual screen-reader review should confirm that live event updates are announced as a bounded status change rather than as a flood of individual rows.

Every interactive control must be reachable by keyboard and must expose a visible focus state. Event tables must remain readable at 320px by stacking, wrapping, or horizontal scrolling without clipping action controls.

## Performance budget

The explorer launch budget is:

| Budget | Limit |
| --- | --- |
| Initial HTML payload | `LAUNCH_MAX_HTML_BYTES`, default 350 KB. |
| Explorer response time | `LAUNCH_MAX_RESPONSE_MS`, default 2500 ms. |
| Event list rendering | Use pagination or virtualization; never render an unbounded history. |
| Live stream updates | Batch or cap updates so high-rate contracts cannot pin the main thread. |

Use the busiest configured testnet contract for the final pass. Synthetic quiet-contract checks are useful for smoke coverage, but they do not prove launch readiness.

## End-to-end smoke journey

The staging journey must prove a launch visitor can:

1. Load the public explorer.
2. Open a real contract view.
3. Query the authenticated events API with an issued key.
4. Open the event stream endpoint and keep the connection alive long enough to receive data or a clean bounded timeout.
5. Identify the failed step and response code when any part breaks.

Run this immediately before launch and after every staging deploy.

## Webhook delivery guarantee

Trident webhooks target at-least-once delivery. The indexer commits each event together with an `event_outbox` row in one transaction, and a relay publishes outbox rows to a Redis stream; delivery workers then consume with `XReadGroup` and record each attempt in `webhook_deliveries`. A non-2xx response or network failure remains retryable until the retry budget is exhausted, after which the delivery is visible as a dead-lettered failure for operator review. One caveat for launch: webhook retries are in-process, so a delivery-worker crash mid-retry strands the entry in the consumer group's pending list — no `XAutoClaim` recovery exists on that path yet. Dead-letter rows are pruned after 7 days, so replay is bounded by that window. See `docs/observability/event-delivery.md`.

Ordering is best-effort per subscription while deliveries succeed on the first attempt. Retries can reorder events because a later event may be delivered before an earlier event finishes its retry schedule. Consumers must deduplicate by event id and treat delivery order as advisory.

Operators should alert on delivery success ratio, dead-letter count, and latency from outbox insertion to successful delivery.
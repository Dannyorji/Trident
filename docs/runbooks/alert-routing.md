# Alert Routing

How Prometheus alerts get from a firing rule to a page on a real device, and
how to verify that routing is working before launch week begins.

See [`docs/runbooks/incident-response.md`](incident-response.md) for the
on-call owner contact details and the severity definitions that determine which
alerts page immediately versus create a ticket.

---

## Severity label to action mapping

Alerts in `monitoring/alerts.yml` and `observability/burn-rate-alerts.yml` use
a `severity` label. The Alertmanager routing tree below turns that label into
a concrete action:

| `severity` label | Action |
|---|---|
| `page` | Immediately routes to the pager integration (PagerDuty / OpsGenie / SMS); wakes the on-call owner at any hour |
| `critical` | Same as `page` — `critical` is the label used in `monitoring/alerts.yml`; `page` is used in `observability/burn-rate-alerts.yml`; both route to the pager receiver |
| `warning` | Routes to the team notification channel (Slack / Discord / email) — no page, no wake-up |
| `ticket` | Routes to the team notification channel — investigate during business hours |

---

## Alertmanager configuration

Below is a minimal `alertmanager.yml` that implements the routing above.
Fill in the `<PLACEHOLDER>` values before deploying. Store secrets (API keys,
webhook URLs) in environment variables or a secrets manager — never commit
them to the repo.

```yaml
# alertmanager.yml
global:
  resolve_timeout: 5m

route:
  receiver: "team-notifications"   # default: warnings and tickets
  group_by: ["alertname", "service"]
  group_wait: 30s
  group_interval: 5m
  repeat_interval: 4h

  routes:
    # Page immediately for critical / page severity
    - match_re:
        severity: "^(critical|page)$"
      receiver: "oncall-pager"
      group_wait: 0s
      repeat_interval: 30m
      continue: false

    # Warning and ticket go to the notification channel only
    - match_re:
        severity: "^(warning|ticket)$"
      receiver: "team-notifications"
      continue: false

receivers:
  - name: "oncall-pager"
    # --- Option A: PagerDuty ---
    pagerduty_configs:
      - service_key: "<PAGERDUTY_SERVICE_INTEGRATION_KEY>"
        description: '{{ template "pagerduty.default.description" . }}'
        severity: '{{ if eq .CommonLabels.severity "page" }}critical{{ else }}{{ .CommonLabels.severity }}{{ end }}'
        details:
          runbook_url: '{{ .CommonAnnotations.runbook_url }}'
          service: '{{ .CommonLabels.service }}'

    # --- Option B: OpsGenie (comment out PagerDuty block above) ---
    # opsgenie_configs:
    #   - api_key: "<OPSGENIE_API_KEY>"
    #     message: '{{ .CommonAnnotations.summary }}'
    #     priority: 'P1'

    # --- Option C: Webhook to a custom endpoint or SMS gateway ---
    # webhook_configs:
    #   - url: "https://<your-webhook-endpoint>/alert"
    #     send_resolved: true

  - name: "team-notifications"
    # --- Slack ---
    slack_configs:
      - api_url: "<SLACK_WEBHOOK_URL>"
        channel: "#trident-alerts"
        title: '[{{ .Status | toUpper }}] {{ .CommonAnnotations.summary }}'
        text: '{{ .CommonAnnotations.description }}'
        send_resolved: true
    # --- Or Discord (via a webhook adapter, e.g. alertmanager-discord) ---
    # webhook_configs:
    #   - url: "https://<discord-webhook-adapter>/alert"
    #     send_resolved: true

inhibit_rules:
  # Suppress the warning-level lag alert when the critical one is already firing
  # for the same instance — reduces duplicate noise.
  - source_match:
      alertname: TridentIndexerLagCritical
    target_match:
      alertname: TridentIndexerLagWarning
    equal: ["instance"]

  # Suppress dependency and pool alerts when the API process is already down —
  # the process-down alert is the signal; the rest is noise.
  - source_match:
      alertname: TridentAPIProcessDown
    target_match_re:
      alertname: "^(TridentAPIDependencyUnhealthy|TridentAPIDBPoolSaturated|TridentAPIHTTP5xxRate.*)$"
    equal: ["job"]

  # Same for the indexer: suppress heartbeat/metrics alerts when process is down.
  - source_match:
      alertname: TridentIndexerProcessDown
    target_match_re:
      alertname: "^(TridentIndexerHeartbeatStale|TridentIndexerMetricsMissing|TridentIndexerLag.*)$"
    equal: ["job"]
```

### Wiring Alertmanager into Prometheus

Add the following to your `prometheus.yml`:

```yaml
alerting:
  alertmanagers:
    - static_configs:
        - targets: ["alertmanager:9093"]
```

If running under the Prometheus Operator, create an `Alertmanager` CRD and a
`Secret` named `alertmanager-<name>` in the same namespace containing the
`alertmanager.yml` above as `alertmanager.yaml`.

---

## Escalation policy (pager tool configuration)

Configure the following escalation policy in PagerDuty / OpsGenie so the
secondary on-call owner is paged automatically if the primary does not
acknowledge:

1. Alert fires → notify **primary on-call** immediately (push notification +
   phone call).
2. If no acknowledgement within **15 minutes** → notify **secondary on-call**
   (push notification + phone call).
3. If no acknowledgement within **30 minutes** → notify the full engineering
   team channel (Slack / Discord `@here`).

Set `repeat_interval: 30m` in the Alertmanager receiver (already set in the
config above) so resolved alerts silence themselves promptly and unresolved
ones re-notify every 30 minutes.

---

## Pre-launch routing test

**Status: not yet executed (issue #620).** The on-call contacts below in
[`incident-response.md`](incident-response.md), the PagerDuty/Slack keys in
this file, and every row of the test table below are still the unexecuted
template — no real device has ever received a page through this path.
Filling them in with real values and actually running the five steps against
real infrastructure and a real team is what turns this from a template into
a completed test, the same distinction
[`docs/LAUNCH_CHECKLIST.md`](../LAUNCH_CHECKLIST.md) draws for the rest of
the launch gate. `scripts/check-oncall-readiness.sh` checks this
mechanically — run it locally to confirm the current state:

```bash
scripts/check-oncall-readiness.sh
```

It exits non-zero while any of the placeholders below remain, and exits 0
once the on-call contacts, the PagerDuty/Slack keys, and all five test rows
are genuinely filled in. It is not yet wired into CI as a required check,
for the same reason `check-launch-gate.sh` isn't either: doing so before
this is real work would fail every PR on a gap this script can only report,
not close.

Run this checklist before launch week. The goal is to confirm that a real
alert produces a real page on a real device — not just that the config file
parses.

### Step 1 — validate config syntax

```bash
amtool check-config alertmanager.yml
promtool check rules monitoring/alerts.yml
promtool check rules observability/burn-rate-alerts.yml
promtool check rules observability/rpc-alerts.yml
```

All three commands must exit 0 with no errors.

### Step 2 — send a test alert through Alertmanager

Use `amtool` to fire a synthetic alert directly at Alertmanager and verify
it reaches the pager receiver:

```bash
# Fire a synthetic SEV-1 alert
amtool alert add \
  --alertmanager.url=http://localhost:9093 \
  alertname=RoutingTest \
  severity=critical \
  service=test \
  summary="Routing test — safe to ignore" \
  description="Sent by pre-launch routing test procedure"

# Confirm it was received
amtool alert query --alertmanager.url=http://localhost:9093 alertname=RoutingTest

# Silence it once you've confirmed the page landed
amtool silence add \
  --alertmanager.url=http://localhost:9093 \
  --duration=10m \
  alertname=RoutingTest
```

Expected result: the on-call owner receives a page (push notification or SMS)
within 1 minute of the `amtool alert add` call.

### Step 3 — verify warning routing

Repeat step 2 with `severity=warning` and confirm the alert appears in the
team notification channel (Slack / Discord) but does **not** produce a page.

### Step 4 — confirm inhibit rules

Fire `TridentIndexerProcessDown` and `TridentIndexerLagWarning` simultaneously
and confirm only the process-down alert appears in the pager — the lag warning
should be suppressed by the inhibit rule.

### Step 5 — escalation test

Acknowledge the test page on the **secondary** owner's device (not the
primary's) and confirm the escalation policy fires correctly after the
15-minute window. This test requires coordination between both on-call owners.
Schedule it explicitly — do not assume the escalation path works without
testing it end-to-end.

### Step 6 — document the test result

Record the outcome here before launch:

| Step | Tester | Date | Result |
|---|---|---|---|
| 1 — config validation | | | pass / fail |
| 2 — SEV-1 page delivered to primary device | | | pass / fail |
| 3 — warning to notification channel only | | | pass / fail |
| 4 — inhibit rules suppress duplicate alerts | | | pass / fail |
| 5 — escalation to secondary after 15 min | | | pass / fail |

Launch must not proceed until all five steps show `pass`.

---

## Silence and maintenance windows

During planned maintenance (deploys, Postgres vacuums, volume resizes), create
a silence in Alertmanager before starting work to avoid noise:

```bash
amtool silence add \
  --alertmanager.url=http://localhost:9093 \
  --duration=30m \
  --comment="Planned deploy $(date -u +%Y-%m-%dT%H:%M)" \
  service=indexer
```

Limit silences to the specific `service` or `alertname` label — silencing
`severity=critical` globally during maintenance is dangerous and masks real
problems.

---

## Runbook URL pattern

Every alert in `monitoring/alerts.yml` sets `runbook_url` to
`docs/runbooks/alerts.md#<anchor>`. If you move or rename the runbook file,
update the `runbook_url` annotations in both alert files to match, or the
URLs embedded in pages will 404.

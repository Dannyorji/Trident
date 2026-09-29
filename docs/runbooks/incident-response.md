# Incident Response

How Trident handles production incidents from detection through resolution and
communication. This document covers: severity classification, on-call ownership
for launch week, the escalation path, and the user communication channel.

For per-alert first steps see [`docs/runbooks/alerts.md`](alerts.md).
For post-incident review see [`docs/runbooks/post-incident-review.md`](post-incident-review.md).
For alert routing and pager configuration see [`docs/runbooks/alert-routing.md`](alert-routing.md).

---

## Severity levels

### SEV-1 — service down or data incorrect

The product is broken for all or a meaningful fraction of users, or data
correctness cannot be guaranteed.

Concrete examples:
- `TridentIndexerProcessDown` or `TridentAPIProcessDown` firing — the indexer
  or API is not reachable.
- `TridentIndexerHeartbeatStale` + `TridentIndexerLagCritical` both firing —
  the poll loop is hung and lag is growing without bound.
- `TridentAPIDependencyUnhealthy` sustained for > 5 minutes — Postgres, Redis,
  or the gRPC backend is down; `GET /v1/health` is failing.
- `IngestFreshnessFastBurn` with `severity: page` — the 28-day error budget
  will exhaust in under 2 days at the current burn rate.
- `TridentDiskFillingWithin48Hours` — the Postgres volume will be full within
  two days; writes are about to fail.
- Any security breach: unauthorized access, credential exposure, or confirmed
  data exfiltration.

Response:
- Page the on-call owner immediately (see below).
- Acknowledge within **15 minutes**.
- Post a user-facing status update within **30 minutes** (see User
  communication channel below).
- Resolve or mitigate within **4 hours** as a target; escalate if not.

### SEV-2 — degraded but functional

The service is running but a component is impaired. Users may notice slowness,
elevated error rates, or stale data, but core functionality still works.

Concrete examples:
- `TridentIndexerLagWarning` firing (lag > 200 ledgers, indexer is behind but
  not stalled).
- `TridentAPIHTTP5xxRateHigh` (5–25% of requests returning 5xx).
- `TridentIndexerRPCErrorRateHigh` (5–25% of Stellar RPC calls failing).
- `TridentRPCHighLatency` (p95 RPC latency > 5s, indexer is slow but alive).
- `TridentRPCFailoverActive` (running on a fallback RPC endpoint).
- `IngestFreshnessSlowBurn` with `severity: ticket` — budget is being consumed
  at 6x but not yet imminently.
- `TridentAPIDBPoolSaturated` (pool > 90% utilised, requests starting to queue).
- `TridentDiskFillingWithin14Days` (provisioning signal, not yet urgent).

Response:
- Notify the on-call owner; no immediate page required if the alert is
  `severity: ticket` or `severity: warning`.
- Acknowledge and begin investigation within **2 hours** during business hours,
  next business day otherwise (unless the SEV-2 is trending toward SEV-1, in
  which case escalate immediately).
- Post a user-facing status update if users are likely to observe the
  degradation (e.g. visible lag increase, elevated error rate on public
  endpoints).
- Resolve or downgrade to a tracked ticket within **24 hours**.

### SEV-3 — operational concern, no user impact

Something is worth investigating but has no current user impact.

Concrete examples:
- `TridentIndexerParseErrorRateHigh` (> 1% of events failing XDR decode —
  parser may need an update but events are isolated, not lost).
- `TridentRPCRateLimited` (quota pressure; indexer is still running).
- `TridentDiskSpaceLow` (< 15% free, predictive alert triggered as backstop).
- A single alert that self-resolved before investigation started.
- Any `severity: ticket` alert that has not worsened in 30 minutes.

Response:
- Create a tracking ticket.
- No immediate notification required.
- Resolve within the current sprint or the next planned maintenance window.

---

## On-call owner — launch week

**Status: not yet named (issue #620).** Both contacts below are still the
unexecuted template — `scripts/check-oncall-readiness.sh` checks for this
placeholder mechanically; see
[`alert-routing.md`'s pre-launch routing test](alert-routing.md#pre-launch-routing-test)
for the rest of what has to be real before launch (PagerDuty/Slack keys, the
routing test itself).

**Primary on-call:** [FILL IN: name, GitHub handle, mobile number or pager
handle — e.g. `@alice`, +1-555-0100, PagerDuty target `alice-trident`]

**Secondary / escalation:** [FILL IN: name, GitHub handle, mobile number or
pager handle — e.g. `@bob`, +1-555-0101, PagerDuty target `bob-trident`]

**Coverage window:** launch week is defined as the 7-day period starting on
the day of the first public announcement. Both contacts above are on-call for
the full window; the primary takes the first page, the secondary is contacted
if the primary does not acknowledge within 15 minutes.

**After launch week:** replace this section with the team's regular on-call
rotation schedule. If a rotation tool (PagerDuty, OpsGenie, etc.) is in use,
link the schedule here instead of keeping a manual list.

---

## Escalation path

1. Alert fires → on-call owner is paged automatically via the routing
   configured in [`docs/runbooks/alert-routing.md`](alert-routing.md).
2. On-call owner acknowledges within **15 minutes** for SEV-1, **2 hours**
   for SEV-2.
3. If the primary does not acknowledge within 15 minutes: the secondary is
   paged (configure this as an escalation policy in your pager tool — see
   alert-routing.md).
4. If neither acknowledges within 30 minutes: any team member who sees the
   alert should take the incident and post a status update.
5. If the incident is not resolved within 4 hours (SEV-1) or 24 hours
   (SEV-2): the secondary owner escalates to the broader engineering team
   via the team's standard Slack/Discord channel with an `@here` mention.
6. For security incidents, follow `SECURITY.md` in parallel — GitHub
   Security Advisories for private triage before any public disclosure.

---

## User communication channel

**Decided:** [FILL IN one of the options below and delete the others before
launch.]

**Option A — GitHub Discussions / Announcements**
Use the `Announcements` category in GitHub Discussions
(`https://github.com/Telocel-Labs/Trident/discussions`). Post a new discussion
thread for each incident with updates appended as comments. Link the thread
from any user-visible status page or README badge.

**Option B — Status page (hosted)**
Use a hosted status page (e.g. statuspage.io, Instatus, or a self-hosted
Cactus/Cachet instance). The on-call owner updates the status and posts
incident messages directly. Set the status page URL in the README and in the
docs site.

**Option C — Discord / Slack `#status` channel**
Use a dedicated `#status` or `#incidents` channel in the project's community
server. Announce incident start, updates, and resolution there. Ensure the
channel is read-only for non-maintainers so the feed is clean.

**What to communicate, in every case:**

- **Incident start:** what is affected, what users may observe, that
  investigation is underway. Do not speculate on root cause yet.
  Template: _"We are investigating an issue affecting [service/feature].
  Users may observe [symptoms]. We will post updates every 30 minutes."_

- **Update (every 30 minutes for SEV-1, every 2 hours for SEV-2):** current
  status, what has been ruled out, next action.

- **Resolution:** what happened (brief, non-technical summary), when normal
  service resumed, whether any data was affected, and a pointer to the
  post-incident review once it is published.

---

## Declaring an incident

Any team member can declare an incident. You do not need certainty — if you
think something is wrong and it might be SEV-1 or SEV-2, declare it and let
investigation prove otherwise. A false alarm is cheaper than a missed one.

To declare:
1. Open a private working channel (e.g. `#incident-YYYY-MM-DD` in Slack/Discord)
   or a GitHub Discussion (draft, not published yet) to coordinate.
2. Assign an incident commander — the person who owns communication and
   coordinates the response. Defaults to the on-call owner.
3. Post the initial user-facing communication (see above).
4. Work the relevant runbook in [`docs/runbooks/alerts.md`](alerts.md).
5. On resolution, post the resolution update and schedule the post-incident
   review within **72 hours** using the template in
   [`docs/runbooks/post-incident-review.md`](post-incident-review.md).

---

## Review cadence

- **After every SEV-1 or SEV-2 incident:** complete a post-incident review
  (see template). Link it from this document's incident log below.
- **Quarterly:** review severity thresholds and on-call owner list for
  accuracy.

## Incident log

| Date | Severity | Summary | PIR |
|---|---|---|---|
| — | — | No incidents yet | — |

# Post-Incident Review (PIR) Template

Copy this file to `docs/runbooks/pir/YYYY-MM-DD-<slug>.md` and fill in each
section within 72 hours of incident resolution. The goal is to understand what
happened and prevent recurrence — not to assign blame.

See [`docs/runbooks/incident-response.md`](incident-response.md) for severity
definitions and the process that led here.

---

## Incident summary

| Field | Value |
|---|---|
| **Date / time (UTC)** | YYYY-MM-DD HH:MM – HH:MM |
| **Duration** | e.g. 47 minutes |
| **Severity** | SEV-1 / SEV-2 / SEV-3 |
| **Incident commander** | @handle |
| **Responders** | @handle, @handle |
| **Services affected** | e.g. indexer, API, Postgres |
| **User impact** | e.g. "All API consumers received stale data for 40 minutes; GET /v1/events returned events up to ledger N while chain was at N+900" |

---

## Timeline

All times UTC. Include the moment of first signal, every significant action or
finding, and the moment the incident was declared resolved.

| Time (UTC) | Event |
|---|---|
| HH:MM | Alert fired: `<AlertName>` on `<instance>` |
| HH:MM | On-call owner acknowledged |
| HH:MM | [action / finding] |
| HH:MM | [action / finding] |
| HH:MM | Mitigation applied: [description] |
| HH:MM | Service confirmed healthy; incident resolved |
| HH:MM | User-facing resolution update posted |

---

## Root cause

Describe the underlying cause in one or two paragraphs. Be specific: which
component failed, why it failed, and what conditions had to be true for the
failure to occur. If the root cause is still uncertain, say so and list the
hypotheses that were ruled out.

---

## Contributing factors

List conditions that made the incident worse or harder to detect, separate from
the root cause itself. Examples: insufficient observability, a missing alert,
a configuration drift, an untested code path, inadequate runbook coverage.

- [factor]
- [factor]

---

## What went well

Things that limited the blast radius or helped resolve the incident faster than
it could have gone.

- [observation]
- [observation]

---

## What went poorly

Things that slowed detection, diagnosis, or resolution.

- [observation]
- [observation]

---

## Action items

Each item must have an owner and a due date. "We should improve X" without an
owner and a deadline is not an action item.

| # | Action | Owner | Due | Issue |
|---|---|---|---|---|
| 1 | [description] | @handle | YYYY-MM-DD | #NNN |
| 2 | [description] | @handle | YYYY-MM-DD | #NNN |

---

## Alert and runbook coverage

Did the existing alerts detect this incident? Were the runbook steps in
[`docs/runbooks/alerts.md`](alerts.md) accurate and sufficient?

- **Alert that fired (or should have fired):** `<AlertName>` — did it fire at
  the right time? Too late? Not at all?
- **Runbook accuracy:** were the first steps in the runbook correct? What was
  missing or misleading?
- **Gap identified:** [describe any new alert, metric, or runbook section
  needed, and link the tracking issue if one was opened]

---

## Communication review

- Was the initial user-facing status update posted within 30 minutes (SEV-1)
  or 2 hours (SEV-2)? If not, why not?
- Was the update cadence (every 30 minutes for SEV-1) maintained?
- Was the resolution message accurate and sufficient?
- Any changes needed to the user communication channel or templates in
  [`docs/runbooks/incident-response.md`](incident-response.md)?

---

## Approvals

PIR should be reviewed by at least one person who was not the incident
commander before being committed.

| Role | Name | Date |
|---|---|---|
| Author | @handle | YYYY-MM-DD |
| Reviewer | @handle | YYYY-MM-DD |

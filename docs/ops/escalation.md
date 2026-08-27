# Escalation matrix

The on-call escalation path for Gregale incident response.

## Tier definitions

- **Tier 0 — Primary oncall (`faas-platform-oncall`)**
  - The first responder for any page-tier alert.
  - Time-to-ack target: 5 minutes.
  - Time-to-engage target: 15 minutes (first command run,
    log line added, or status-page update posted).

- **Tier 1 — Secondary oncall**
  - Paged if the Tier 0 ack window (15 minutes) elapses
    without acknowledgement, OR if the primary oncall
    explicitly hands off (e.g. needs sleep, in another
    incident, hardware failure on the oncall workstation).
  - Time-to-engage target: 15 minutes from page.
  - Secondary oncall rotation is in PagerDuty under
    `faas-platform-oncall-secondary`.

- **Tier 2 — Engineering manager**
  - Paged if both Tier 0 and Tier 1 ack windows elapse (30
    minutes total from the initial page), OR for any
    customer-facing-impact SEV1 (full fleet outage, data
    loss, security incident).
  - The engineering manager has authority to: pull
    additional engineers from other projects, escalate to
    the CEO for customer-communication decisions, engage
    legal for security incidents, engage the hosting
    provider (Hetzner for the EX44 deploy) for hardware
    issues.

## Customer-communication escalation

- **Tier 0**: post to status page (via the `faas-status`
  automation in `cmd/statusd/` — the oncall types the
  summary into the oncall Slack channel and the bot
  mirrors it).
- **Tier 1**: customer-facing email blast via the
  `customer-comms` SendGrid template (templated per the
  `pkg/customercomms/` package — pre-approved subject
  lines only).
- **Tier 2**: hold all customer comms until the
  engineering manager approves the wording.

## Postmortem cadence

- SEV1 (customer-facing impact >15 min): postmortem
  within 5 business days.
- SEV2 (no customer impact, but operator-visible
  incident >30 min): postmortem within 10 business days.
- SEV3 (deferred incident, batched): reviewed at the next
  weekly retro.

## Oncall handoff

The handoff document is in `docs/ops/handoff-template.md`
(not yet written — see follow-up ticket). The oncall MUST
write the handoff doc BEFORE going off rotation, covering:

1. Active incidents (status, ETA, customer impact).
2. Open follow-up tickets from prior incidents.
3. Pending alerts that haven't paged yet (silent
   degradation).
4. Upcoming scheduled maintenance windows.
5. Anything unusual in the past week (deploys, traffic
   anomalies, customer escalation patterns).

## When to break the matrix

If a SEV1 is in progress and Tier 0 is stuck (no ack for
>10 minutes), Tier 1 should NOT wait the full 15 min —
page immediately. The matrix is a guideline; the goal is
fast customer-impact mitigation, not procedure adherence.
# BillingDrift

Source: `deploy/ansible/roles/prometheus/files/faas.rules.yml`,
the `BillingDrift` + `BillingDriftAccount` alert blocks
(ADR-049 §B.1).

Metrics: `meterd_billing_drift_mb_seconds{account_id, provider}`
(signed local − pushed) and `meterd_billing_drift_ratio{account_id,
provider}` (abs(drift) / max(local, pushed)) over the rolling
24 h window. Emitted by `pkg/billing/reconciler` (PR-B), which
runs on a 6 h meterd cron tick (default `FAAS_RECONCILE_INTERVAL`).

Severity: `page` (fleet-wide) and `warn` (per-account). The page
severity fires on `> 0.5%` fleet-wide drift for `> 1h` — a real
Stripe or Paddle outage has the potential to silently lose
invoices. The per-account `warn` alert catches single-customer
drift early so an operator can investigate before the customer
notices.

## Symptom

The alert fires when the reconciler's drift ratio exceeds the
threshold over the rolling window:

| Alert | Trigger | For | Severity |
|---|---|---|---|
| `BillingDrift` | `max by (provider) (meterd_billing_drift_ratio) > 0.005` | 1h | page |
| `BillingDriftAccount` | `meterd_billing_drift_ratio > 0.05` | 15m | warn |

The `provider` label distinguishes Stripe vs Paddle. The
`account_id` label carries the offending customer id (omitted on
the fleet-wide rule, which aggregates by `provider`).

A sustained drift in one direction (e.g. local > pushed) means the
provider is missing usage records; the other direction (pushed >
local) is rarer and indicates duplicate pushes — both are revenue
or reputational risks.

## Verify

The reconciler is read-only against the provider surface. The
local-vs-pushed comparison runs in `pkg/billing/reconciler` on
every `FAAS_RECONCILE_INTERVAL` tick (default 6 h). The Pusher
(`pkg/meter/pusher.go`) is the write-side complement — it runs on
the daily Stripe cadence.

> **First tick after restart has zero drift.** The reconciler reads
> `usage_minutes` over a 24 h window, but the provider-side query
> returns the same window's records. After a meterd restart the
> local sum is the existing minute-grain rows; the provider's sum
> lags by up to one pusher cadence (24 h). On the very first tick,
> the ratio shows the per-account drift as if it were a real
> divergence — this is a startup artifact and self-corrects on the
> next pusher push.

For ad-hoc queries:

```bash
# Current per-account drift (the warn alert's source expression)
curl -fsS 'http://127.0.0.1:9090/api/v1/query?query=meterd_billing_drift_ratio' | jq .

# Fleet-wide drift by provider (the page alert's source expression)
curl -fsS 'http://127.0.0.1:9090/api/v1/query?query=max by (provider) (meterd_billing_drift_ratio)' | jq .

# Signed drift in mb_seconds — direction tells you which side is
# under-reported
curl -fsS 'http://127.0.0.1:9090/api/v1/query?query=meterd_billing_drift_mb_seconds' | jq .

# Per-account detail — replace $ACCOUNT_ID
ACCOUNT_ID=<uuid>
curl -fsS --data-urlencode "query=meterd_billing_drift_mb_seconds{account_id=\"${ACCOUNT_ID}\"}" \
  'http://127.0.0.1:9090/api/v1/query' | jq .

# Stripe-side summary list (last 24h, the matching window)
# This is the API call pkg/billing/reconciler would make in the
# follow-up PR that swaps the (0, nil) stub for a real
# stripe.UsageRecordSummaries.list query.
stripe usage_record_summaries list \
  --subscription_item <sub_item_id> \
  --start $(date -u -d '24 hours ago' +%s) \
  --end   $(date -u +%s)
```

## Check

```bash
# Confirm meterd is running the reconciler cron (look for the
# six-hour tick — recent journal shows the latest reconcile)
journalctl -u meterd --since '-7h' --no-pager | grep -i "reconcile\|drift"

# Recent meterd logs for any per-account reconcile failures
# (these are fail-soft warnings; the loop continues)
journalctl -u meterd --since '-7h' --no-pager | grep -i "reconcile account failed"

# Confirm the reconciler is registered with the /metrics endpoint
curl -fsS http://127.0.0.1:9090/metrics | grep meterd_billing_drift

# Cross-check the local usage_minutes sum for the offending
# account (replace $ACCOUNT_ID)
ACCOUNT_ID=<uuid>
psql -t -A -c "select sum(mb_seconds) from usage_minutes where account_id = '${ACCOUNT_ID}' and minute >= now() - interval '24 hours'"
```

A drift paired with low `meterd_billing_drift_mb_seconds` per-account
count indicates a transient provider blip (Stripe 5xx, Paddle
rate-limit) — fail-soft handles these; the next tick re-queries.
A drift paired with the same `account_id` firing repeatedly across
multiple ticks means the provider is missing records for that
customer — escalate.

A fleet-wide `BillingDrift` on Stripe but not on Paddle means
Stripe is the affected surface (Stripe outages are far more common
than Paddle's). A fleet-wide drift on both providers is rare and
indicates an upstream Postgres issue (the local sum would be the
side most likely wrong) — check meterd's DB connectivity first.

## Silence

```bash
# Silence a known Stripe outage window — wait for the Stripe
# status page to recover before removing the silence.
amtool silence add \
  --matchers='alertname="BillingDrift",provider="stripe"' \
  --duration=2h \
  --comment='Stripe status page reports incident; tracking on https://status.stripe.com'

# Silence per-account drift for a customer migrating plans
# (their billing surface is in flux; the drift is expected)
ACCOUNT_ID=<uuid>
amtool silence add \
  --matchers='alertname="BillingDriftAccount",account_id="'"${ACCOUNT_ID}"'"' \
  --duration=24h \
  --comment='customer mid-migration; provider summary endpoint lags'
```

## Recover

Three-step cascade, ordered from least to most disruptive:

1. **Identify the affected provider.** The fleet-wide rule's
   `provider` label tells you immediately. For Stripe: check
   [status.stripe.com](https://status.stripe.com) for an active
   incident. For Paddle: check the Paddle status page. If the
   provider reports an outage, the recovery is on their side —
   silence the alert (above) and wait.

2. **Reconcile manually for the affected window.** If the provider
   is up but `meterd_billing_drift_mb_seconds` is non-zero,
   re-trigger the pusher by running `faas billing push --account
   <id> --window "last 24 hours"` (out-of-band CLI shipped in
   a follow-up PR). For a fleet-wide provider outage, the daily
   pusher's idempotency-key contract means the next successful
   tick covers the gap — no manual replay needed. Cross-check by
   waiting one pusher interval (24 h) and re-querying the drift
   gauge; it should fall below the threshold.

3. **Escalate: contact provider support with the per-account
   query results.** For a per-account `BillingDriftAccount` that
   persists beyond a 7-day window, the local-vs-pushed mismatch
   is real and unrecoverable from the box side. Open a support
   ticket with the provider, attach the `meterd_billing_drift_mb_seconds`
   series for that account, and request a manual replay of the
   missing usage records.

Recovery verification:

```bash
# After the provider recovers: the drift ratio should drop
# below 0.005 within one pusher cadence (24 h) + one reconcile
# cadence (6 h) = ~30 h. Verify with:
curl -fsS 'http://127.0.0.1:9090/api/v1/query?query=max by (provider) (meterd_billing_drift_ratio)' | jq .

# After a manual push replay: per-account drift ratio drops
# below 0.05 within one reconcile tick (~6 h).
curl -fsS 'http://127.0.0.1:9090/api/v1/query?query=meterd_billing_drift_ratio' | jq .
```

A sustained recovery (no further `BillingDrift` or
`BillingDriftAccount` fires for 24h) closes the incident; the
silence expires on its own and the gauge surfaces the next
genuine drift. Memory pressure is **not** part of this runbook —
`FaasHighResidentRamPct` governs memory; this runbook governs
provider-side billing surface only.
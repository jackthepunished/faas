# ADR-042 — Webhook signature replay protection

Status: Accepted, 2026-07-28. Owner: @poyrazK. Closes issue #294.
Related: spec §5.1 (audit events), §11 (security rules), ADR-035
(auth audit events), ADR-021 (grace timer — sweep cadence precedent).

## Context

The three webhook ingresses on the box (GitHub via gatewayd, Stripe +
Paddle via apid) verify HMAC signatures but never check the delivery
UUID against a dedupe table. A replayed (re-POSTed) webhook within the
provider's signature-validity window succeeds twice — the handler runs
its side effects (charge-refunded, plan-change, dunning email,
cron-fired, etc.) a second time.

| Provider | Header / field checked | Today | Risk |
|---|---|---|---|
| GitHub | `X-Hub-Signature-256` (`pkg/githubd/webhook.go:31`) | HMAC only | replayed push within seconds re-runs the deploy pipeline |
| Stripe | `Stripe-Signature` (`pkg/billing/stripe/client.go:236`) | HMAC only (5min tolerance) | replayed `charge.refunded` double-credits; replayed `customer.subscription.deleted` re-suspends |
| Paddle | `Paddle-Signature` (`pkg/billing/paddle/webhook.go:153`) | HMAC only (5min tolerance) | replayed transaction events double-bill |

SOC 2 CC6.1 expects idempotency on every external event ingestion.
The existing dedupe tables — `stripe_push_dedupe` (migration 00004)
and `paddle_overage_dedupe` (migration 00034) — are pusher-side
(meterd), not webhook-replay side. The pusher dedupe guards against
double-billing when meterd retries; it does NOT guard against the
upstream provider re-POSTing a webhook to apid.

## Decision

Add a single shared `webhook_deliveries` table, one helper package
(`pkg/webhookdedupe`), one audit event kind (`webhook.replay_rejected`),
and a 5-minute TTL window. Replays are rejected with 200 (idempotent —
the upstream provider interprets as success and stops retrying).

### The table — migration `00059_webhook_deliveries.sql`

```sql
create table if not exists webhook_deliveries (
  provider     text        not null,
  delivery_id  text        not null,
  received_at  timestamptz not null default now(),
  expires_at   timestamptz not null,
  primary key (provider, delivery_id),
  constraint  webhook_deliveries_provider_check
    check (provider in ('github','stripe','paddle'))
);

create index if not exists webhook_deliveries_expires_idx
  on webhook_deliveries (expires_at)
  where expires_at is not null;
```

- `provider` is `text` with a CHECK rather than an enum so adding a
  future provider is a one-line ALTER (mirrors the `kind` convention
  in the `events` table).
- `delivery_id` is `text` because Paddle `event_id` is a UUID-string
  and Stripe `event.id` is `evt_…` (long string); `uuid` would be
  wrong.
- Partial index on `expires_at` keeps the sweep DELETE cheap even at
  large N (rows older than 5min get swept; rows younger than 5min
  stay).

### TTL = 5 minutes

Matches the Stripe / Paddle signature tolerance windows already
used by their verifiers (`5*time.Minute` in `pkg/billing/stripe` and
`pkg/billing/paddle`). A legitimate retry that falls inside the
signature-validity window cannot bypass the replay check.

GitHub redelivers failed deliveries for up to 24h; a 5min TTL is a
tighter window. The HMAC check is still in force across that wider
window — legitimate retries after apid/gatewayd restart bypass the
dedupe but are still authenticated. Acceptable for v1; a wider TTL
is a follow-up if needed.

### Response semantics — 200 on replay

The replay handler returns 200 (idempotent — the upstream provider
interprets as success and stops retrying). This matches the
existing `unknown customer` and `unknown event type` patterns at
`handlers_ext.go:1148-1154` and `:1206-1213` — Stripe, Paddle, and
GitHub all interpret 2xx as success.

### Audit event — `webhook.replay_rejected`

Emitted by both gatewayd and apid via their respective audit seams
(cmd/gatewayd/audit.go + cmd/apid/audit.go). ADR-035 best-effort
semantics: failure to write the audit row does NOT roll back the
200-on-replay response. Subject is the resolved customer account
id for apid; `nil` for gatewayd (the proxy has no account id at
the edge). Data payload carries `{provider, delivery_id}` so a
dashboard filter (`kind_prefix=webhook.`) can scope by provider.

### Sweep

A single goroutine in apid (`pkg/webhookdedupe.Sweeper`) issues
`delete from webhook_deliveries where expires_at < now()` every 60s.
gatewayd does NOT run a sweep — apid owns the table for Stripe +
Paddle, and gatewayd's inserts get swept by apid's goroutine. The
partial index on `expires_at` keeps the DELETE O(N expired) rather
than O(N total).

The 60s cadence matches the meterd dunning sweep
(`pkg/meter/dunning.go:223`). The webhook_deliveries table is much
smaller than the dunning work so the same cadence is fine.

### Fail-open on transport errors

The dedupe check fails open (log WARN + forward) on connection /
transport errors. The dedupe is defence-in-depth, not the
authenticity gate — the HMAC verify above is the gate. This matches
the gatewayd fail-open posture at `githubd_proxy.go:checkReplay`
and the apid posture at `handlers_ext.go:1188` (Stripe) and
`:1263` (Paddle).

## Alternatives considered

1. **Per-provider dedupe tables** (stripe_deliveries,
   paddle_deliveries, github_deliveries). Rejected: three tables to
   migrate, sweep, and audit; the dedupe semantics are identical
   across providers; the audit event kind already names the provider.
2. **24h TTL** (matches GitHub redelivery window). Rejected: too
   many rows to sweep; the HMAC check still gates authenticity
   across the wider window; a follow-up can raise TTL if needed.
3. **Per-customer replay windows.** Rejected: adds complexity
   (per-account config), the issue body is silent on per-account
   limits, and uniform TTL matches the signature-validity window
   for all three providers.

## Conventions honoured

- **Migration slots renumbered at PR creation** — issue body says
  `00052`, `origin/main` ends at `00055`, slots 56–58 contested by
  open PRs #335/#352/#369; this ADR ships at slot `00059` (next
  free above 58). Memory:
  `migration-slot-renumber-at-pr-creation.md` + PR #377 gate.
- **SQL via sqlc** — the dedupe pair is added to `queries.sql` and
  regenerated; pgstore wraps the sqlc helpers (mirrors
  `stripe_push_dedupe` pair at `pgstore.go:3783-3806`).
- **Best-effort audit (ADR-035)** — `webhook.replay_rejected`
  emission never rolls back the 200-on-replay response. Pin via
  `cmd/apid/handlers_audit_test.go::TestAuditEvents_CronCreatedFreeReturns402DoesNotEmit`
  precedent.
- **Handlers ≤ 50 lines — extract** — `stripeWebhook` is currently
  ~80 lines after the replay check; the next refactor can extract
  `verifyAndDedupStripeWebhook` to stay under 50 on the handler
  itself. Out of scope for #294 (current shape is still readable).
- **CodeQL `go/log-injection`** — `delivery_id` is
  provider-supplied but pre-verified by HMAC; routes through
  `log/slog`'s attribute-pair form, never a format string.

## Out of scope

- Webhook payload-version rejection (separate issue).
- Per-app webhook secret rotation (separate issue).
- Encrypted-storage / HSM-backed secret retrieval (gap G2, §17).
- Webhook delivery UUID format validation (GitHub's UUIDv4 format,
  Stripe's `evt_…` format). The dedupe table accepts any string
  and the upstream HMAC check is the authenticity gate.
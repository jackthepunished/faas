# ADR-134 · Mail pipeline production hardening (issue #246 closure)

- **Status:** accepted
- **Date:** 2026-08-29
- **Closes:** issue #246 (tier-1 ship blocker), unblocks #245
  (email verification), ADR-045 (webhook-only customer alerts),
  ADR-123 (`notification_email`).
- **Supersedes:** draft PR #446 (fork-authored, 113 LOC) —
  covered the ansible wiring (item 1 of the original six).
- **Builds on:** ADR-115 (provider selection + ADR-115 D5
  credential fail-closed) — this ADR extends D5 from "credential
  missing" to "transport unselected" and adds the bounce /
  compliance / observability work ADR-115 deferred.

## Context

ADR-115 named Resend as the transactional-email provider and
tightened the boot path against missing credentials. The
transports themselves (`pkg/mail/resend.go` / `postmark.go`)
were already production-ready; what was missing was everything
*around* delivery. The audit on main `fc7136b67` (issue #246
audit) found five gaps that collectively meant a production
box could boot healthy and journal the entire 4-step dunning
ladder instead of sending it:

1. **Silent drop on unset transport.** `pkg/mail/factory.go`
   resolved an unset `FAAS_MAIL_TRANSPORT` to `LogSender` at
   INFO. The default branch did the same for a typo (`resned`).
   Production boxes booted healthy.
2. **Nothing selects a transport.** `grep -rn FAAS_MAIL deploy/ansible/`
   returned zero hits — the only mention of mail in deploy/
   was a commented-out block in `sealed.env.example`.
3. **No 429 / retry / idempotency.** Both senders mapped only
   5xx → `ErrTransient`. Resend's free tier is 100/day, so 429
   was an operational certainty and classified permanent.
4. **No bounce ingest.** Hard bounces never suppressed or fed
   dunning. A bounced-and-ignored email was the worst-case
   silent failure mode.
5. **No bulk-sender compliance.** `Message.Headers` was on the
   struct with zero callers; `List-Unsubscribe` (RFC 8058) was
   absent. Gmail / Yahoo would junk every quota-warning mail.

Issue #246 was tagged tier-1 ship-blocker. Issue #245 (email
verification at signup) was blocked on it. ADR-045 made
customer alerts webhook-only *because* of it, and ADR-123
deferred `notification_email` to the same unblock.

## Decision

### D1. Decorator stack — fail-fast at the outermost layer.

Wire the outbound mail path as a closed decorator chain in
`cmd/apid/main.go` and `cmd/meterd/main.go`:

```
SuppressingSender      outermost — gate on the suppression list, drop entire message
  └── RetryingSender    429/5xx backoff with full jitter; 5s wall-clock cap
        └── ResendSender | PostmarkSender | LogSender | NoopSender
```

The chain is asserted by `pkg/mail/stack_test.go` so a future
"simplification" that pulls a decorator out fails loudly. The
suppression check is outermost so a suppressed address costs
zero HTTP attempts — re-trying a hard bounce against the same
address wastes the daily Resend free-tier quota.

The decorator stack gets all 10 production send call sites
correct at once (`pkg/meter/dunning.go:248,264`,
`pkg/meter/quota.go:148`, `cmd/apid/handlers_auth_login.go`,
`cmd/apid/handlers_mfa.go`, `cmd/apid/handlers_account.go`,
`cmd/apid/handlers_ext.go`, `pkg/grace/grace.go`). The
alternative — patching each call site — is a 10-file PR with
no structural guarantee that the next send site will get it
right.

### D2. Fail-closed on unset / unknown transport in production.

Extend ADR-115 D5 from "credential missing" to "transport
unselected". `pkg/mail/factory.go::SenderFromEnv` returns
`ErrMailUnsetInProd` when `FAAS_MAIL_TRANSPORT` is unset on a
non-dev box (`FAAS_DEV` unset/`0`), and `ErrMailUnknownTransport`
when the value isn't in the closed set
`{resend, postmark, log, noop}`. Both errors propagate out of
`runWithDeps` so apid + meterd refuse to boot. The escape
hatch is `FAAS_DEV=1` (resolves to LogSender) or an explicit
`log`/`noop` value.

The ansible control-plane role mirrors the contract at
playbook time — `deploy/ansible/roles/control_plane_service/tasks/main.yml::control_plane_service — assert FAAS_MAIL_TRANSPORT selected`
fails the playbook run when the sealed.env row is missing or
set to `log` on a non-dev box. Defence in depth: the daemon
won't boot even if the ansible role is bypassed.

### D3. Resend webhook ingress — Svix HMAC + dedupe.

Resend uses Svix / Standard Webhooks for bounce / complaint /
delivery events (issue #246 acceptance item 8). The verifier
(`pkg/mail/webhook_signature.go`) parses
`svix-signature: v1,<base64> [v1,<base64> …]` against
HMAC-SHA256(`<svix-id>.<svix-timestamp>.<body>`), base64-decodes
the secret after stripping the `whsec_` prefix, enforces a
5-min timestamp tolerance, and returns wrapped `ErrBadSignature`
on any failure. Multi-version headers are accepted — Svix
reserves the right to add v2 / v3 and the verifier must keep
v1 working through the migration.

The handler lives at `POST /v1/webhooks/resend`, mounted
unwrapped (no auth middleware — the HMAC IS the trust
boundary), next to the Paddle route. It fails closed with
503 when `FAAS_MAIL_RESEND_WEBHOOK_SECRET` is unset so a
missing env var cannot silently accept unsigned events.
Replay dedupe is via `webhookdedupe.CheckReplay` keyed on
`svix-id`; a redelivery within the 5-min TTL returns 200
(idempotent — Resend stops retrying) + a
`webhook.replay_rejected` audit row.

The route is registered in BOTH
`cmd/apid/spec_compliance_test.go::routeExclude` AND
`cmd/sdk-coverage/main.go::routeExclude`. Paddle is currently
in only one of them; missing either is a CI gate failure.

### D4. Bounce → existing dunning, no second state machine.

Hard bounces flow into `pkg/meter.BounceHandler`, which
upserts a `mail_suppressions` row (unique on
`(source, provider_event_id)`), emits a `mail.bounce_hard`
audit row, and calls the existing
`Store.MarkDunningStep(active → past_due)`. No second state
machine, no new `AccountStatus`. The CAS returns
`ErrNotFound` when the status doesn't match — that's the
existing redelivery-race guard, so replays are free.
Complaints suppress but do NOT transition — suspending an
account because the recipient hit "spam" is hostile.

Cross-process plumbing: the apid handler dispatches the bounce
into the local meterd-owned `BounceHandler` via the
`mailBounceHandler` interface in
`cmd/apid/handlers_mail_webhooks.go`. Today this is a local
in-process call (apid and meterd co-locate on a single
control-plane node); the consumer-package interface leaves a
clean seam for an RPC adapter once meterd ships on its own
node.

### D5. List-Unsubscribe on the quota-warning template only.

`pkg/mail/headers.go::MarketingHeadersMap(url)` returns the
RFC 8058 pair (`List-Unsubscribe: <url>` +
`List-Unsubscribe-Post: List-Unsubscribe=One-Click`). The
quota-warning template carries both headers when an operator
has wired `FAAS_NOTIFICATIONS_UNSUBSCRIBE_URL` via meterd's
boot-time fail-closed validator.

**Deliberate deviation from the obvious reading:** dunning /
billing / deletion mails do NOT carry the headers. Gmail /
Yahoo bulk-sender rules target *promotional* mail; a customer
who one-click-unsubscribes from "your payment failed" stops
receiving the suspension warning and gets deleted silently —
the exact failure #246 exists to prevent. The header
*infrastructure* is generic; the policy table lives in
`pkg/mail/headers.go` with the rationale per template, and
`pkg/mail/renderer_transport_integration_test.go::TestRendererToTransport_DunningReachesWireWithoutUnsubscribe`
pins the policy in unit.

### D6. `ResendRequest.Headers` is the channel for custom headers.

The pre-PR transport set custom headers via `req.Header.Set`,
which the real Resend API silently drops. The fix moves the
`Headers` field onto the `ResendRequest` JSON body
(`pkg/mail/resend.go::ResendRequest.Headers`), which Resend
treats as opaque and attaches verbatim to the outbound
message. The integration test
`pkg/mail/renderer_transport_integration_test.go` asserts the
end-to-end path so a future "simplification" can't regress it.

### D7. Operator dry-run + renderer→transport integration tests.

`gregale mail dry-run [--unsubscribe-url URL]` renders every
production template against a synthetic fixture account + day
and writes the wire payload as JSON to stdout. Operators run
this on a staging box before flipping production to
`FAAS_MAIL_TRANSPORT=resend` — it's the eyeball gate for the
bulk-sender compliance work (issue #246 acceptance item 6).

The renderer→transport integration tests
(`pkg/mail/renderer_transport_integration_test.go`) stand up
an httptest Resend stub and assert each template reaches the
wire with subject/body/headers intact — the §14 milestone
gate the spec has never had. Pre-PR the test would have failed
on the `List-Unsubscribe` field because of D6.

### D8. Ansible wiring + playbook-level fail-closed.

`deploy/ansible/roles/control_plane_service/tasks/main.yml`
gains a `control_plane_service — assert FAAS_MAIL_TRANSPORT selected`
task that fails the playbook run when the sealed.env row is
missing or `log` on a non-dev box. Mirrors the daemon-level
fail-closed (D2) so the operator sees the gap at playbook
time, not after `systemctl restart faas-apid` shows
`failed`.

`deploy/controlplane/sealed.env.example` gains the
`FAAS_MAIL_RESEND_WEBHOOK_SECRET` row next to the existing
mail block, with a comment block pointing at the Resend
dashboard's "Signing Secret" field.

## Consequences

### Positive

- **#246 closed in full.** All 6 acceptance items ship in this
  PR.
- **#245 unblocked.** Email verification at signup can now
  reuse `pkg/mail.Sender` instead of depending on a missing
  provider.
- **ADR-045 + ADR-123 unblocked.** Customer alerts can
  reference `pkg/mail` for the `notification_email` surface
  instead of working around its absence.
- **Single PR, structural change.** The decorator stack gets
  all 10 send sites correct at once and a future new call site
  inherits the chain automatically.

### Negative / follow-ups

- **DNS automation deferred.** Operators add the SPF / DKIM /
  DMARC records by hand at the DNS provider
  (`docs/ops/mail-deliverability-dns.md`). A real automation
  budget is a separate ADR.
- **Postmark webhook parity deferred.** Today only the Resend
  dispatch is wired (`cmd/apid/handlers_mail_webhooks.go`).
  Postmark uses a different signature scheme; a follow-up
  PR mirrors the handler. The transport itself (send path) is
  symmetric.
- **In-process dedupe state.** `webhookdedupe` is a
  process-local sync.Map; a daemon restart clears the
  dedupe window, so a replay arriving within 5 minutes of
  restart can pass through. Acceptable for v1 because the HMAC
  verify in front of it is the authenticity gate. A follow-up
  PR backs the dedupe with a shared `webhook_deliveries`
  table.
- **Migration slot 00525.** The `mail_suppressions` table
  collides with open PR #1185's 517-524 slot fence pattern.
  Verified uncontested at push time via
  `scripts/ci/check_migration_slots.sh`; the slot landscape
  moves daily and a re-verify is part of the merge gate.

## See also

- ADR-115 (`docs/adr/115-transactional-email-provider-resend.md`)
  — the selection + D5 fail-closed contract this ADR extends.
- ADR-032 v2 (`docs/adr/032-paddle-billing-provider.md`) —
  the env-var-selector + pluggable-facade pattern the decorator
  stack mirrors.
- ADR-035 (auth audit events) — the audit-row contract
  `mail.bounce_hard` / `mail.bounce_complaint` follow.
- ADR-094 (`webhook replay dedupe`) — the `webhookdedupe`
  primitive the Resend handler uses.
- `docs/ops/mail-deliverability-dns.md` — the operator DNS
  runbook.
- `docs/compliance/subprocessors.json` — the
  `transactional-email-resend` entry expanded to document the
  inbound webhook data flow (toward #296).

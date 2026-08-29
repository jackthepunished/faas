# ADR-115 · Transactional email provider — choose Resend (spec G4 closure)

- **Status:** accepted
- **Date:** 2026-08-18
- **Closes:** spec §17 G4 (`docs/faas_implementation_spec.md:1231`)
  — "Transactional email provider — verification, dunning, quota mails
  reference email; no provider". Decide-by date was M7.
- **Depends on:** ADR-032 v2 (`docs/adr/032-paddle-billing-provider.md`,
  the env-var-selector + pluggable-facade pattern this decision mirrors
  for outbound mail).
- **Related:** ADR-089 (per-secret rotation; rotation of
  `FAAS_MAIL_RESEND_API_KEY` is explicitly deferred, see §D.2), ADR-091
  (edge rules — Resend webhooks will reuse the `/webhooks/<provider>`
  shape; deferred, see §D.3), ADR-035 (audit log surface — mail-send
  failures will eventually emit a `mail.send_failed` audit row; deferred
  to ADR-118).

## Context

Spec §17 G4 names the gap: verification, dunning, and quota mails
reference email, but no provider was chosen. The M7 dunning state
machine (`pkg/meter/dunning.go`), the password-reset + magic-link flows
(`cmd/apid/handlers_auth_login.go`), and the quota-warning loop
(`pkg/meter/quota.go`) all deliver through `pkg/mail`; the G4 closure
is the *selection* of which production transport the
`FAAS_MAIL_TRANSPORT` selector resolves to, plus the hardening of the
boot path against misconfig.

`pkg/mail` already ships:

- `Sender` interface (`pkg/mail/mail.go:42`) and `Message` struct with
  `HTMLBody` + `Headers` fields (`mail.go:29-36`).
- `SenderFromEnv(getenv, log) (Sender, error)` factory
  (`pkg/mail/factory.go`) selecting on `FAAS_MAIL_TRANSPORT` ∈
  {`resend`, `postmark`, `log`, `noop`} (case-insensitive). Unknown /
  unset defaults to `LogSender` (fail-soft for dev / CI). Live transports
  selected with the credential env var empty return
  `(nil, ErrMailerMisconfigured)` (ADR-115 §D5 fail-closed).
- `ResendSender` (`pkg/mail/resend.go`) — full HTTP API client
  (`POST https://api.resend.com/emails`, `Authorization: Bearer …`,
  `{from, to, subject, text, html?}`), 10 s timeout, `ErrTransient` on
  5xx / network, 1 MiB body cap, `logsanitize` on every field.
- `PostmarkSender` (`pkg/mail/postmark.go`) — full Postmark client
  with identical retry + sanitisation semantics.
- Two wiring points — `cmd/apid/main.go` (through `newMailerAdapter` →
  apid-local `Mailer` interface at `cmd/apid/server.go:481-493`) and
  `cmd/meterd/main.go` (direct into `meter.NewDunning`). Both refuse
  to boot when `SenderFromEnv` returns `ErrMailerMisconfigured`.
- 10 production send sites across the four email surfaces
  (verification, dunning, quota, hard-delete complete) — none reference
  a concrete transport.
- `pkg/mail/factory_test.go` + `cmd/apid/mail_wiring_test.go` — full
  table-driven coverage of every `FAAS_MAIL_TRANSPORT` branch including
  the two fail-closed rows.
- `pkg/wire/secrets_audit_test.go` — covers `FAAS_MAIL_RESEND_API_KEY`
  + `FAAS_MAIL_POSTMARK_TOKEN` in the secrets audit (one row per
  env-var description, not a credential string).
- Sub-processor compliance — both Resend and Postmark are pre-listed
  in `docs/compliance/subprocessors.md:28-29` and the
  `docs/DPA.md:150-155` sub-processor schedule. No DPA amendment is
  required for this selection.
- Operator template — `deploy/controlplane/sealed.env.example`
  carries a `# --- transactional email (ADR-115 / spec G4 closure) ---`
  block with the four `FAAS_MAIL_*` rows, the verified-domain
  requirement, the key-scoping requirement, and a pointer to this ADR.

The G4 closure is a *selection + boot-hardening*, not a new
implementation. The transport itself was already merged; this ADR names
the choice and tightens the contract.

## Decision

### D1. Name Resend as the production transactional-email provider.

Resend's free tier — 3,000 emails / month, 100 / day — covers the M7
dunning + verification + quota-mail volume with substantial headroom;
the paid tier ($20/mo for 50k) is a clean capacity-step. Postmark's
free tier — 100 / month, 25 to test senders — is operationally a
non-starter for dunning alone (a single past-due wave on a non-trivial
customer base exhausts it). The two providers are otherwise comparable
on HTTP API ergonomics, observability surface, and per-message header
support.

### D2. Pin the `pkg/mail` interface contract.

`Sender` interface, `Message` struct, the env-var selector
**`FAAS_MAIL_TRANSPORT`** (case-insensitive; `resend` / `postmark` /
`log` / `noop`), and the per-provider credentials —
**`FAAS_MAIL_RESEND_API_KEY`** + **`FAAS_MAIL_FROM`** for Resend,
**`FAAS_MAIL_POSTMARK_TOKEN`** + **`FAAS_MAIL_FROM`** for Postmark —
are the load-bearing names. No breaking change to the existing 10
production send sites (`cmd/apid/handlers_auth_login.go:304,853`,
`cmd/apid/handlers_mfa.go:431`, `cmd/apid/handlers_account.go:237`,
`cmd/apid/handlers_ext.go:2895,2947`, `pkg/meter/dunning.go:248,264`,
`pkg/grace/grace.go:213`, `pkg/meter/quota.go:148`); the selection is
a configuration change, not a code change.

The default transport stays `log` when `FAAS_MAIL_TRANSPORT` is unset
/ empty. **The default flip to `resend` does not ship in this ADR** —
it lands in the ADR-116 follow-on so this ADR remains a pure
selection decision reviewable in isolation.

Postmark remains operator-selectable via
**`FAAS_MAIL_TRANSPORT=postmark`** for parity with the `pkg/mail`
interface contract; this preserves the operator's option to swap
providers without a code change, which is the same posture ADR-032 v2
established for billing providers.

### D3. Document the verified-domain requirement.

The `From` address set in **`FAAS_MAIL_FROM`** must belong to a domain
whose SPF + DKIM records are published at the DNS provider. Resend
rejects with HTTP 403 and a `validation_error` body if the From domain
is not on the verified-domains list. The operator adds the records via
the Resend dashboard during setup; the values surface in the
`deploy/controlplane/sealed.env.example` comment block (ADR-115 §D6)
so operators have a checklist at deploy time. DNS automation is a
separate Ansible role (deferred, §D.5).

### D4. Document the operator-side key-scoping requirement.

The Resend API key issued for **`FAAS_MAIL_RESEND_API_KEY`** must be
scoped to "Sending access" only in the Resend dashboard (not full
account access). This is the smallest-privilege posture the Resend key
model supports; an over-scoped key ("Full access") would let a leaked
key manage domains, revoke other keys, and read the API-key list. The
`pkg/wire/secrets_audit_test.go` row for `FAAS_MAIL_RESEND_API_KEY`
records the "Sending access only" constraint in the description string
so it surfaces in the operator-facing secrets audit.

### D5. Fail-closed on missing credential when a live transport is selected.

Today the factory (`pkg/mail/factory.go:53-56` pre-#115) warns +
falls back to `LogSender` when `FAAS_MAIL_TRANSPORT=resend` is
selected but `FAAS_MAIL_RESEND_API_KEY` is empty. This ADR flips that
branch to return `(nil, ErrMailerMisconfigured)` wrapped with
`ErrResendMissingAPIKey` / `ErrPostmarkMissingToken`. apid +
`cmd/meterd/main.go` propagate the error out of `runWithDeps` so the
daemon refuses to boot. `systemctl status` shows `failed`; the
operator cannot run a Gregale node that silently drops email into
slog.

The unset-default (`FAAS_MAIL_TRANSPORT` empty) and the
unknown-transport branches stay fail-soft to `LogSender` — those are
dev / CI defaults, not production misconfig. The fail-closed contract
fires only when an operator has explicitly asked for a live transport.

### D6. Template `FAAS_MAIL_*` rows in `sealed.env.example`.

`deploy/controlplane/sealed.env.example` gains a
`# --- transactional email (ADR-115 / spec G4 closure) ---` block with
comment-annotated rows for `FAAS_MAIL_TRANSPORT`,
`FAAS_MAIL_RESEND_API_KEY`, `FAAS_MAIL_FROM`, and (as a fallback
option) `FAAS_MAIL_POSTMARK_TOKEN`. The comment block names the
verified-domain requirement (D3), the key-scoping requirement (D4),
the fail-closed contract (D5), and the sub-processor posture. Operators
have a copy-pasteable template at deploy time.

### D7. Default stays `log` when `FAAS_MAIL_TRANSPORT` is empty.

The default flip to `resend` ships in the ADR-116 follow-on once
operators have had a chance to land their own credentials +
verified-domain setup. This ADR keeps the unset-default fail-soft
behavior so dev / CI / unit tests continue to work, and so operators
can land this cluster without flipping production traffic.

## Consequences

### Positive

- The 10 production send sites deliver email end-to-end on any Gregale
  deployment once the operator sets three env vars
  (`FAAS_MAIL_TRANSPORT=resend`, `FAAS_MAIL_RESEND_API_KEY`,
  `FAAS_MAIL_FROM`) and verifies the From domain. No code, no
  migration, no per-handler branching.
- Resend's per-message `Idempotency-Key` header is accepted by the API
  and stored for 24 h, which gives the dunning state machine free
  at-least-once safety if a retry races a slow upstream. (Wired in a
  follow-up, see §D.2.)
- The sub-processor list is unchanged — both providers are pre-listed;
  the G4 closure is a configuration change inside the existing
  compliance envelope.
- The fail-closed contract (D5) eliminates the silent-fallback-to-log
  failure mode that today's `pkg/mail/factory.go` warns about. An
  operator-selected live transport without the credential now produces
  a clean boot refusal + a one-line ERROR log naming the missing
  env var.

### Negative

- A second vendor in the outbound surface; the operator now has a
  separate key-rotation surface (Resend dashboard) for
  `FAAS_MAIL_RESEND_API_KEY`. ADR-089's sealed-secret walker does not
  cover mail keys today; that gap is recorded in §D.2.
- The fail-closed contract (D5) is a behavior change: an operator who
  was relying on the silent-fallback-to-log for misconfig recovery
  must now set the credential or flip back to
  `FAAS_MAIL_TRANSPORT=log`. The unset-default stays fail-soft; only
  the explicit live-transport case is fail-closed.
- `cmd/apid/server.go::Message` (the apid-local mirror at lines
  481-493) does NOT have the `Headers` field that `pkg/mail.Message`
  has; widening the apid-local type so Gmail/Yahoo bulk-sender
  compliance headers can flow through is in §D.4.

### Compatibility

The selection is additive on the load-bearing contracts. Every existing
apid + meterd binary keeps booting with `FAAS_MAIL_TRANSPORT=log` (the
unset default); no operator is forced to flip. The 10 send sites
continue to call `pkg/mail`; only the resolved `Sender` implementation
changes when the env var is set. The factory's signature widened from
`SenderFromEnv(...) Sender` to `SenderFromEnv(...) (Sender, error)` —
the only callers (`cmd/apid/main.go` + `cmd/meterd/main.go`) were
updated in this PR.

### Rollback

The operator unsets `FAAS_MAIL_TRANSPORT` (or sets it to `log`) and
restarts apid + meterd. The `Sender` interface, the `Message` struct,
the 10 send sites, and the factory selector are unchanged — only the
active transport flips. No data migration, no schema change, no env-var
rename. The Resend API key remains valid in the Resend dashboard and
can be re-deployed by re-setting the env var.

The fail-closed contract (D5) can be reverted independently by
re-flipping the `case TransportResend` / `case TransportPostmark`
branches in `pkg/mail/factory.go` to the warn-and-LogSender path; no
call-site changes are required because the failure-path branches are
encapsulated in the factory.

## Files

### New

- `docs/adr/115-transactional-email-provider-resend.md` — this ADR.
- `pkg/mail/factory_test.go` — table-driven factory contract tests
  (8 cases covering unset / explicit / noop / resend-with-key /
  postmark-with-token / resend-without-key-fails-closed /
  postmark-without-token-fails-closed / bogus-transport).

### Modified

- `pkg/mail/factory.go` — D5 fail-closed contract: live transports
  with missing credentials now return `(nil, ErrMailerMisconfigured)`
  wrapped with the underlying config error. New `ErrMailerMisconfigured`
  sentinel. Signature widened to `(Sender, error)`. Unset-default +
  unknown-transport stay fail-soft.
- `pkg/mail/resend.go` — `NewResendSender` returns the
  `ErrResendMissingAPIKey` sentinel (was a literal error string) so
  `errors.Is(err, mail.ErrResendMissingAPIKey)` works.
- `pkg/mail/postmark.go` — `NewPostmarkSender` returns the
  `ErrPostmarkMissingToken` sentinel (was a literal error string).
- `pkg/mail/transports_test.go` — two existing rows flipped from
  fail-soft to fail-closed (`TestSenderFromEnv_ResendFailsClosedOnMissingAPIKey`,
  `TestSenderFromEnv_PostmarkFailsClosedOnMissingToken`); existing
  happy-path tests updated to consume the new `(Sender, error)` return.
- `cmd/apid/main.go` — boot path now propagates the
  `ErrMailerMisconfigured` error out of `runWithDeps` so apid refuses
  to boot when the factory returns it. Hint log names the env vars to
  set.
- `cmd/apid/mail_wiring_test.go` — table restructured to assert
  fail-closed on the two misconfig rows + happy-path on the rest;
  the adapter-nil-collapse branch is no longer exercised on the
  fail-closed rows (boot exits before reaching the adapter).
- `cmd/meterd/main.go` — same boot-path pattern as apid.
- `deploy/controlplane/sealed.env.example` — D6 contract surface: the
  four `FAAS_MAIL_*` rows + comment block.
- `docs/adr/README.md` — one row appended to the Log table after the
  ADR-114 row.

## Rejected alternatives

- **Postmark as primary.** Postmark's free tier (100 / month, 25 to
  test senders) cannot carry the dunning + verification + quota-mail
  volume of even a small launch. The paid tier starts at $15 / month
  for 10k emails vs. Resend's $0 / 3k → $20 / 50k sliding scale.
  Postmark remains an operator-selectable fallback via
  `FAAS_MAIL_TRANSPORT=postmark` (D2).
- **Self-hosted SMTP / Mailgun / Amazon SES.** Self-hosted SMTP
  imposes a maintenance burden (queue mgmt, IP warming, bounce
  handling) the operator does not have the headcount for in the
  year-one launch. Mailgun's smallest paid plan starts at €35 / month
  — an order of magnitude over the €3 / month line the financial model
  already budgets. SES is cheaper per-message but adds a sender-domain
  warm-up runway (typically 2-4 weeks) that does not fit the M7
  ship-blocker.
- **No provider (keep `LogSender` as the production transport).**
  Rejected because the spec mandates email delivery for dunning — the
  M7 acceptance gate is that a past-due account receives the
  suspension notice by email, not via the slog stream. The
  `LogSender` is for dev / staging only.
- **Fail-soft misconfig (keep the pre-#115 `WARN + LogSender`
  behaviour).** Rejected because the operator can land a Gregale node
  with `FAAS_MAIL_TRANSPORT=resend` + a missing key and silently lose
  every dunning + magic-link email to the slog stream. The financial
  model depends on dunning notices landing; the spec mandates email
  delivery for M7. The fail-closed contract (D5) makes the boot
  refuse-to-start behaviour visible to the operator at deploy time.

## Deferred

The four follow-on items in scope for the post-selection PR cluster
(each gets its own ADR slot or follow-up issue; none block this ADR's
selection decision):

- **§D.1 — Default flip to `resend`** when `FAAS_MAIL_TRANSPORT` is
  empty. Lands in ADR-116 once operators have had a chance to land
  their own credentials + verified-domain setup. This ADR keeps the
  unset-default fail-soft behavior so dev / CI still works.
- **§D.2 — API-key rotation / multi-key support.** Today
  `ResendConfig.APIKey` (`pkg/mail/resend.go:35-41`) is a single
  string; rotation requires a restart with the new value. ADR-089
  covers sealed-secret re-keying for `pkg/secretbox`-stored secrets
  but does not cover `pkg/mail` API keys. **Suggested ADR slot 117.**
  An `Idempotency-Key` header on the Resend wire (so the dunning
  state machine can retry safely across rotations) ships in the same
  PR.
- **§D.3 — Bounce / complaint webhook ingest.** No
  `pkg/mail/webhook*.go` and no `/webhooks/resend` route in apid
  today. The follow-on adds a `POST /v1/webhooks/resend` handler with
  HMAC-SHA256 signature verification (per the `docs/DPA.md:181`
  webhook pattern), a `resend_deliveries` table (provider + delivery_id
  + 24 h TTL, mirroring the `webhook_deliveries` shape from
  ADR-042 / G11), and a single sweep goroutine in apid that emits
  `mail.bounce` + `mail.complaint` audit rows so accounts that
  hard-bounce get auto-suspended by the existing dunning state
  machine. **Suggested ADR slot 118; PR-cluster mirrors the ADR-042
  webhook cluster shape.**
- **§D.4 — HTML body templates + `List-Unsubscribe` / `Reply-To`
  headers for Gmail/Yahoo bulk-sender compliance.**
  `pkg/mail.Message.Headers` field exists (`mail.go:35`) but no caller
  populates it; `cmd/apid/server.go::Message` (apid-local mirror at
  lines 481-493) does NOT have the `Headers` field — both would need
  widening. Today zero production sends use HTML. **Suggested ADR slot
  119.** Out of scope for G4.
- **§D.5 — DNS verification automation.** SPF / DKIM records for the
  `From` domain. Separate Ansible role under `deploy/ansible/` (the
  existing `dns` / `sealed_env` roles are the right neighbours).
  **Operator runbook issue, not an ADR.**
- **§D.6 — `MFADisableEmailRequestedBody`** (`pkg/mail/mfa.go:137-164`).
  Staged for MFA PR 2, not G4 scope. **No ADR slot reserved;
  existing MFA cluster owns it.**

## Verification

- `make test` — must stay green. The updated `cmd/apid/mail_wiring_test.go`
  + the new `pkg/mail/factory_test.go` cover the fail-closed branch.
  The 10 production send-sites are unchanged; their tests (under
  `cmd/apid/`, `pkg/meter/`, `pkg/grace/`) remain green.
- `make lint` — must stay green. No new exports beyond the
  `ErrMailerMisconfigured` sentinel in `pkg/mail`.
- **Boot-refusal smoke test (fail-closed):**
  1. Set `FAAS_MAIL_TRANSPORT=resend` with `FAAS_MAIL_RESEND_API_KEY`
     intentionally empty.
  2. `systemctl restart faas-apid` (and `faas-meterd`) →
     apid/meterd log a single ERROR record naming
     `transport=resend err="mail: transport misconfigured: mail: Resend
     APIKey required"` and exit non-zero. `systemctl status faas-apid`
     shows `failed`.
  3. Set `FAAS_MAIL_TRANSPORT=resend` + a valid key → both daemons
     boot cleanly; slog INFO record `mail.transport transport=resend`
     fires.
  4. Set `FAAS_MAIL_TRANSPORT=` (empty) → both daemons boot, fall
     through to `LogSender` (unchanged behavior — dev default
     preserved).
- **`sealed.env.example` template check:**
  - `cat deploy/controlplane/sealed.env.example | grep -A1
    'transactional email'` → shows the four `FAAS_MAIL_*` rows with
    the comment block.
  - `make bootstrap` (or the v2 `gregalectl manifest validate`
    equivalent) does not error on the new rows.
- **No-default-flip check:**
  - On a node with no `FAAS_MAIL_TRANSPORT` set, after the PR lands,
    `systemctl status faas-apid` shows `active (running)` and
    `journalctl -u faas-apid | grep mail.transport` shows
    `transport=log` (NOT `transport=resend`). This proves the default
    flip is held back per the D7 + §D.1 decision.
- **Compliance unchanged:**
  - `docs/compliance/subprocessors.md` still has both Resend + Postmark
    rows (no edit needed).
  - `docs/DPA.md:150-155` still accurate.

## Cross-references

- **Spec:** `docs/faas_implementation_spec.md:1231` (§17 G4 — the gap
  row this ADR closes).
- **Code:**
  - `pkg/mail/mail.go` (Sender, Message, LogSender, NoopSender,
    ErrTransient).
  - `pkg/mail/factory.go` (SenderFromEnv selector + fail-closed D5).
  - `pkg/mail/resend.go` (ResendConfig, ResendSender, ResendRequest,
    NewResendSender).
  - `pkg/mail/postmark.go` (the operator-selectable fallback).
  - `pkg/mail/factory_test.go` (new — table-driven contract tests).
  - `pkg/mail/transports_test.go` (per-transport wire tests +
    SenderFromEnv happy-path + fail-closed).
  - `cmd/apid/main.go` (apid wiring → `newMailerAdapter`, fail-closed
    boot path).
  - `cmd/apid/server.go:481-493` (apid-local `Mailer` interface).
  - `cmd/apid/mail_wiring_test.go` (8 transport-selection cases +
    adapter + magic-link flow; restructured for fail-closed).
  - `cmd/meterd/main.go` (meterd wiring → `meter.NewDunning`,
    fail-closed boot path).
  - `pkg/wire/secrets_audit_test.go` (secrets-audit row for
    `FAAS_MAIL_RESEND_API_KEY`).
  - 10 production send sites (all already wired through `pkg/mail`;
    no edits this ADR): `cmd/apid/handlers_auth_login.go:304,853`,
    `cmd/apid/handlers_mfa.go:431`,
    `cmd/apid/handlers_account.go:237`,
    `cmd/apid/handlers_ext.go:2895,2947`,
    `pkg/meter/dunning.go:248,264`,
    `pkg/grace/grace.go:213`,
    `pkg/meter/quota.go:148`.
- **Operator template:**
  - `deploy/controlplane/sealed.env.example` (D6 — the four
    `FAAS_MAIL_*` rows + comment block).
- **Compliance:**
  - `docs/compliance/subprocessors.md:28-29` (Resend + Postmark
    pre-listed).
  - `docs/DPA.md:150-155` (sub-processor schedule; no amendment
    needed).
  - `docs/DPA.md:181` (webhook signature verification pattern; reused
    by §D.3).
- **Related ADRs:**
  - ADR-032 v2 (`docs/adr/032-paddle-billing-provider.md`) — the
    env-var-selector + pluggable-facade pattern this decision mirrors
    for outbound mail.
  - ADR-089 (`docs/adr/089-secret-rotation.md`) — covers sealed-secret
    re-keying; §D.2 calls out the mail-key gap.
  - ADR-091 (edge rules) — the `/webhooks/<provider>` shape that
    §D.3 will reuse.
  - ADR-035 (auth audit events) — the audit-row contract that §D.3's
    `mail.bounce` / `mail.complaint` rows will follow.
  - ADR-042 (webhook replay dedupe) — the `webhook_deliveries` table
    shape that §D.3 mirrors as `resend_deliveries`.
## Acceptance amendment (2026-08-29)

Status flipped proposed → accepted after the mail-production-ready
mega-PR landed the following against main:

- **Decorator stack** (SuppressingSender → RetryingSender →
  ResendSender|PostmarkSender|LogSender|NoopSender) wired in
  cmd/apid/main.go and cmd/meterd/main.go.
- **Fail-closed boot** — `pkg/mail/factory.go` rejects an unset or
  unrecognised `FAAS_MAIL_TRANSPORT` on a non-dev box
  (`FAAS_DEV` unset/`0`). `FAAS_DEV=1` keeps LogSender; explicit
  `log`/`noop` remain valid escape hatches.
- **Suppression list** (`migrations/00525_mail_suppressions.sql`),
  60s in-process cache on the decorator, unique index on
  `(source, provider_event_id)`.
- **List-Unsubscribe** (RFC 8058) wired into the quota-warning
  template only — dunning/billing/deletion mails deliberately
  exclude the header so a customer one-click-unsubscribing from
  "your payment failed" cannot silently lose their suspension
  warning.
- **Resend webhook ingress** at `POST /v1/webhooks/resend` —
  Svix HMAC-SHA256, replay-guarded by `webhookdedupe`, dispatched
  to `meter.BounceHandler` → suppression + dunning CAS via
  `MarkDunningStep(active → past_due)`.
- **Operator dry-run** (`gregale mail dry-run [--unsubscribe-url URL]`)
  + renderer→transport integration tests proving every template
  reaches the wire with subject/body/headers intact.

Issue **#246** closed in full. Issue **#245** (email verification)
unblocked. ADR-045 (webhook-only customer alerts) and ADR-123
(`notification_email`) can now reference the mail pipeline
instead of working around its absence.

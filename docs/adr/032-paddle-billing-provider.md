# ADR-032 · Paddle Billing v2 as the production billing provider (v2)

- **Status:** accepted
- **Date:** 2026-08-18
- **Supersedes:** [ADR-032 v1](032-paddle-billing-provider-v1.md) (the
  opt-in posture, kept as historical reference).
- **Related:** ADR-025 (provider-pluggable billing layer), §14 M7 acceptance.
  The financial-model re-validation against Paddle's MoR fee shape is
  operator-owned and deferred (decision recorded here so the deviation is
  traceable).

## Context

The v1 ADR (2026-07-24) added Paddle as an **opt-in** secondary surface
behind `FAAS_BILLING_PROVIDER=paddle`, with the standing decision that
Stripe remained the production default. PR-P3 / PR-P4 closed the four
M7 dunning transitions, the cross-process overage dedupe, the per-account
catalog hydration, and the operator runbook. The Paddle provider shipped
at ≈5,000 LOC with ≈3,500 LOC of tests (50-iteration property test for the
catalog invariant, the four-test live sandbox walk, the three webhook
e2e cases) — feature-complete at the §14 M7 acceptance surface.

The launch customer base is geographically mixed (per the founding
whitepaper) and the operator's home-country card issuers don't always
process USD-denominated Stripe charges. Paddle's MoR model absorbs VAT
globally, which collapses a class of failure modes that would otherwise
be a manual ops cost per region. The financial model spreadsheet
(operator-owned) is the source of truth for unit-economics math; the
re-validation against Paddle's fee shape is in flight and tracked
outside this ADR.

**There are no existing customers on Stripe at the time of this ADR.**
A `cus_…` to `ctm_…` migration is therefore not in scope. The legacy
Stripe module stays in the workspace (admin endpoints + tests still
reference it) and the apid Stripe surface is still bootable from
`FAAS_BILLING_PROVIDER=stripe` for a node-level opt-out, but the default
flips to Paddle.

## Decision

1. **`FAAS_BILLING_PROVIDER`** defaults to `paddle` at the daemon boot
   loader (`pkg/billing/loader/loader.go`). Empty / unset → Paddle. The
   apid inverse-fallback path (`loader.go:313-318`, which returns
   `nil, m.Name, nil` for the legacy Stripe surface) is **not** the
   default; it remains operational for a node-level rollback.

2. **The deploy template is Paddle-only.** `deploy/controlplane/sealed.env.example`
   no longer ships the `STRIPE_API_KEY` / `STRIPE_WEBHOOK_SECRET` /
   `FAAS_BILLING_PORTAL_URL` rows. `deploy/ansible/roles/control_plane_service/files/{apid,meterd}.toml.example`
   document `[billing.paddle]` as the active block and comment out
   `[billing.stripe]`. `deploy/scripts/verify-secrets.sh` drops the
   Stripe-side grep checks and keeps the Paddle row + the
   `FAAS_BILLING_PROVIDER=paddle` assertion.

3. **`pkg/billing/paddle/errors.go::ClassifyPushError`** (verified —
   no code change required for v2). The Paddle classifier was landed
   in a follow-up to PR-P3 on main (the `paddle-full-enable` cluster,
   PR #204 follow-up). The classifier maps a Paddle push failure to a
   closed 13-label set covering pre-SDK sentinels, transport errors,
   and `*paddleerr.Error` Status codes. The meterd pusher
   (`pkg/meter/pusher.go:208`) dispatches on the concrete provider type
   and emits the label into `meterd_ops_total{op="paddle",code=…}`;
   `pkg/wire/metrics.go:2725` pre-instantiates the histogram labels
   from `paddle.PushResultLabels()` at registry init so the dashboard
   panel renders even before the first push. The v1 "Negative/deferred"
   bullet for the classifier is therefore closed at v2; the launch
   cluster re-verifies the wiring with a focused test rather than
   re-introducing the classifier.

4. **The Idempotency-Key SDK transport wrapper** (PR-E) wires the
   existing `paddle.NewIdempotencyRT` RoundTripper into
   `provider.go:NewProvider` so every Paddle write request carries
   `Idempotency-Key: faas-overage-<acctID>-<YYYY-MM>`. Closes the v1 §4
   "Negative/deferred" bullet.

5. **`.github/workflows/paddle-sandbox.yml`** (PR-D) is the operator-only
   live CI sibling of `stripex-sandbox.yml`. It triggers on
   `workflow_dispatch` and runs the four `paddle_sandbox_e2e` tests
   against `api.sandbox.paddle.com` using the `PADDLE_SANDBOX_API_KEY`
   + `PADDLE_SANDBOX_WEBHOOK_SECRET` repository secrets.

6. **The §14 M7 acceptance gate** is provider-agnostic (invoice shadow
   math < 0.1 % delta, function hello-world p95 wake < 1 s, Free-tier hard
   stop). The Paddle implementation satisfies all three. The sandbox
   walk is the launch gate, not the M7 gate.

7. **`meterd_push_duration_seconds`** keeps the histogram with the
   `provider` label; the `result` label stays `success | failure`.
   `meterd_ops_total{op,code}` is the per-error-class counter, with
   `code` populated by the Paddle classifier for symmetry with Stripe.

## Consequences

### Positive

- Launch customer base is not gated on a single EU entity or a single
  USD card-issuer path. VAT is handled by Paddle's MoR.
- The `pkg/billing.Provider` interface (ADR-025) is the single seam; a
  future Stripe / LemonSqueezy / Braintree plugin is additive — no
  per-handler branching, no migration path through the daemons.
- The two v1 "Negative/deferred" bullets (Paddle error classifier +
  Idempotency-Key transport) close in this cluster, so the operator
  dashboard has Stripe-equivalent label fidelity on day one.

### Negative / deferred

- The Stripe module stays in the workspace. The `stripe-go v70.15.0+incompatible`
  pin in `go.mod:28` is unchanged; the conventional-modules lint gate
  in `.github/workflows/ci.yml:149-156` is relaxed for the `stripe`
  module (one-line edit documented in PR-D's diff) because the module
  is no longer a runtime dependency.
- The financial-model re-validation is operator-owned and out of scope
  for this PR cluster. The model is the source of truth for unit
  economics; if it disagrees with Paddle's fee shape, the deviation is
  resolved outside this ADR.
- PR-5 (Paddle dashboard surface for `paddle_checkout_url` rendering)
  is deferred to a post-launch PR. The dashboard currently redirects
  customers to `paddle_checkout_url` via the apid 402 response.
- The `stripe-go` Stripe-side telemetry counter parities
  (`<daemon>_stripe_webhook_verify_failed_total`) are intentionally not
  added: Stripe is no longer on the runtime path and a parity counter
  with no metric source is dead weight.

### Rollback

A node-level rollback is intentional and trivial:

1. Set `FAAS_BILLING_PROVIDER=stripe` in `sealed.env` on the affected node.
2. Restart `apid` + `meterd`.
3. The legacy apid Stripe surface boots (`loader.go:313-318` returns
   `nil, m.Name, nil` for the legacy surface; the apid reads
   `STRIPE_*` env vars inline at `cmd/apid/main.go:959`).
4. No schema migration is required. `accounts.provider_customer_id`
   carries both `cus_…` (Stripe) and `ctm_…` (Paddle) values
   (migration 00040 already renamed `stripe_customer_id`).

Existing Paddle customers' `ctm_…` IDs are **not** migrated on rollback;
the Stripe side will create fresh `cus_…` IDs on next checkout. The
customer's billing dashboard will show two records until the operator
backfills manually. There are no existing customers at launch, so this
is a documented failure mode, not a launch blocker.

## PR split (the launch cluster)

The cluster ships as one mega-PR (`release/paddle-default-mega`) with six
atomic commits, mirroring the PR-924 / PR-926 / PR-929 / PR-936 mega-PR
pattern the repo has been using for the recent Tier A cluster work:

- **PR-A (this ADR + spec + ops docs)** — no code changes. The four
  doc edits land first because they are the dry-run precondition for
  everything else.
- **PR-B (provider default flip)** — two-line change in
  `pkg/billing/loader/loader.go`, the loader test, the sealed.env
  template scrub, the TOML example flip, the `verify-secrets.sh`
  Stripe-row removal, and the ansible operator assert.
- **PR-C (Paddle error classifier)** — `pkg/billing/paddle/errors.go`
  `ClassifyPushError` + tests; the `ClassifyError` interface method on
  `billing.Provider`; the meter pusher wiring.
- **PR-D (`paddle-sandbox.yml` CI workflow)** — mirrors
  `stripex-sandbox.yml` with Paddle secrets + the `paddle_sandbox_e2e`
  build-tag gate.
- **PR-E (Idempotency-Key SDK transport wrapper)** — wires
  `paddle.NewIdempotencyRT` into `provider.go:NewProvider`; tests assert
  the header stamp on writes.
- **PR-F (launch gate + checklist)** — `docs/ops/launch-checklist.md`
  with the four manual checks; the operator runs them and signs the
  checklist before the v1.0.0 tag.

## Verification

- `make test` (all packages green, including
  `pkg/billing/loader/loader_test.go`, `pkg/billing/paddle/errors_test.go`,
  `pkg/billing/paddle/transport_test.go`, `pkg/meter/pusher_test.go`).
- `make e2e` (the `paddle_e2e_test.go` signed-webhook ingress, three cases).
- `make e2e-sandbox` (the four `paddle_sandbox_e2e` tests against
  `api.sandbox.paddle.com` with `secrets/.env.sandbox` populated from
  the sandbox merchant dashboard).
- `.github/workflows/paddle-sandbox.yml` triggered from the GitHub UI.
- `make doctor-paddle` against a fresh Lima box.
- `docs/ops/launch-checklist.md` signed by the maintainer.

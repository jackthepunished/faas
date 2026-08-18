# Launch checklist — Paddle-default at v1.0

This is the **operator gate** that the v1.0 release (Paddle as the
production billing provider, ADR-032 v2) must pass before the
`v1.0.0` tag is cut. The four checks are sequential; each one
must be green before the next starts. The maintainer who runs the
checklist signs each row with their handle and the date.

> **Pre-req for all four checks:** secrets/.env.sandbox must be
> populated from the `pdl_sandbox_…` key + the matching webhook secret
> from the Paddle sandbox dashboard. The file is `.gitignored`; never
> commit it.

---

## Pre-launch verification gates

### 1. `make e2e-sandbox` — operator-only live walk

- **What:** the four-test sandbox walk against `api.sandbox.paddle.com`
  from `cmd/e2e/billing_paddle_sandbox_test.go`.
- **Why:** the machine-readable CI cannot prove the wire-up against a
  real merchant. The four tests cover CheckoutURL creation,
  subscription.created-stamps-customer-id, transaction.completed-is-noop,
  and per-window-claim round-trip (the meterd production path).
- **How:** `make e2e-sandbox` against a fresh Lima box (the same
  provisioning `make metal-lima` uses) with `secrets/.env.sandbox`
  populated. Requires `DATABASE_URL` and `FAAS_PADDLE_SANDBOX_E2E=1`.
- **Pass criterion:** all four tests pass with no `verify_failed`
  lines in the test logs.
- **Signed:** ____________________  date: _________

### 2. `.github/workflows/paddle-sandbox.yml` — CI workflow

- **What:** the operator-only CI sibling of `stripex-sandbox.yml`,
  triggered from the GitHub Actions UI.
- **Why:** proves the workflow file behaves identically in CI to the
  local `make e2e-sandbox` walk, so a future operator can debug via
  the GitHub Actions logs.
- **How:** with `PADDLE_SANDBOX_API_KEY` + `PADDLE_SANDBOX_WEBHOOK_SECRET`
  set as repository secrets, dispatch the workflow from the UI.
- **Pass criterion:** the workflow runs to completion with all four
  tests `PASS`.
- **Signed:** ____________________  date: _________

### 3. `make doctor-paddle` — operator smoke

- **What:** the 60s `faas billing status --watch` + journal tail
  defined in the Makefile.
- **Why:** the doctor validates that the loader chooses Paddle, the
  catalog hydration completed, and the webhook verify path has not
  surfaced any `paddle_webhook.verify_failed` lines in the last five
  minutes.
- **How:** `make doctor-paddle` against a fresh Lima box (the same
  provisioning `make metal-lima` uses). The output should show
  `provider: paddle` in the JSON status and zero `verify_failed`
  journal lines.
- **Pass criterion:** status JSON shows `provider: paddle`, no
  `verify_failed` lines, and the catalog `lastSyncAt` is recent.
- **Signed:** ____________________  date: _________

### 4. Loader flip — sanity boot

- **What:** re-boot apid + meterd on a sandbox-tagged box and watch
  the boot log for `billing provider loaded provider=paddle`.
- **Why:** the loader change in PR-B is a two-line flip; this
  confirms the env-overlaid TOML + the loader default agree on Paddle.
- **How:** `systemctl restart faas-apid faas-meterd` on a box with
  `FAAS_PADDLE_SANDBOX=1` + `FAAS_PADDLE_API_KEY=pdl_sandbox_…` and no
  `FAAS_BILLING_PROVIDER` set. Grep the boot log for `provider=paddle`.
- **Pass criterion:** the boot log line `billing provider loaded provider=paddle`
  appears within 5 s of the daemons coming up.
- **Signed:** ____________________  date: _________

---

## Post-launch rollback (if Paddle misbehaves in production)

The rollback is intentionally trivial — the legacy Stripe surface is
still bootable per node without a migration:

1. **Set `FAAS_BILLING_PROVIDER=stripe`** in `sealed.env` on the
   affected node.
2. **Restart `apid` + `meterd`** with the legacy `STRIPE_*` keys
   restored from the operator's cold-storage backup.
3. **The Stripe module** is still in the workspace and the apid
   Stripe path is still wired (`pkg/billing/loader/loader.go:313-318`
   returns `nil, m.Name, nil` for the legacy surface).
4. **No schema migration** is required. `accounts.provider_customer_id`
   carries both `cus_…` (Stripe) and `ctm_…` (Paddle) values
   (migration 00040 already renamed `stripe_customer_id`).
5. **Existing Paddle customers' `ctm_…` IDs are not migrated** on
   rollback; the Stripe side will create fresh `cus_…` IDs on next
   checkout. The customer's billing dashboard will show two records
   until the operator backfills manually. There are no existing
   customers at launch, so this is a documented failure mode, not a
   launch blocker.
6. **The accept-time gate that Paddle is the default** is re-evaluated
   in a follow-up ADR if the rollback was triggered by a Paddle-side
   failure mode that affects production traffic.

The rollback path is intentionally simple (no `stripe-go` removal).
The invariant is: the legacy apid Stripe surface is always bootable
from `FAAS_BILLING_PROVIDER=stripe`, even if Paddle is the production
default.

---

## Final sign-off

All four gates green. The maintainer signs the v1.0.0 tag push.

- **Maintainer:** ____________________
- **Date:** ____________________
- **`v1.0.0` tag:** pushed at ____________________
- **`release.yml` flow:** see
  `.github/workflows/release.yml` — the tag push triggers the
  cross-build + SHA256SUMS + moving-tag update + GitHub Release
  attach. The pattern is documented in
  `docs/adr/093-*` and the `release.yml` header comment.

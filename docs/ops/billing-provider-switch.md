# Billing provider switch (Paddle / Polar → Stripe legacy opt-in)

The launch production billing provider is **Paddle Billing v2** (ADR-032 v2,
accepted 2026-08-18). The legacy Stripe surface is still bootable from
`FAAS_BILLING_PROVIDER=stripe` for a node-level opt-out, but the
deploy template is Paddle-only. This runbook covers:

- The selector semantics in both directions.
- The Polar MoR setup and its event-based usage-billing contract.
- The cutover procedure for a node-level rollback to Stripe (the
  on-launch option, intended for a single-node hot-fix while the
  broader cluster rolls forward).
- The four `launch-checklist` gates a maintainer must run before the
  v1.0.0 tag.

> **Operator reminder (v2):** at launch there are no production
> customers on Stripe. The pinned `stripe-go v70.15.0+incompatible`
> module is preserved for admin endpoints + tests; only the runtime
> dispatch is unwired.

## Selector

| `FAAS_BILLING_PROVIDER` | Behavior                                         |
|-------------------------|--------------------------------------------------|
| (empty) / unset         | Paddle (default — production billing provider).  |
| `paddle`                | Paddle (explicit). Requires `FAAS_PADDLE_*`.    |
| `polar`                 | Polar (explicit). Requires `FAAS_POLAR_*` plus product IDs. |
| `stripe`                | Stripe (legacy opt-in for a node-level rollback). Requires `STRIPE_*`. |
| anything else           | Daemon fails to boot with a typed error.         |

The selector is the canonical name for both `apid` and `meterd`; both
daemons import the same loader (`pkg/billing/loader`) so a config drift
between them cannot happen.

## Env vars

### Stripe (default — pre-existing)

| Var                       | Required         | Used by                  |
|---------------------------|------------------|--------------------------|
| `STRIPE_API_KEY`          | Yes (paid plans) | apid + meterd            |
| `STRIPE_WEBHOOK_SECRET`   | Yes              | apid (`/v1/webhooks/stripe`) |
| `FAAS_BILLING_PORTAL_URL` | Recommended      | apid (changePlan 402 template) |

### Paddle (opt-in)

| Var                          | Required | Used by                                       |
|------------------------------|----------|-----------------------------------------------|
| `FAAS_PADDLE_API_KEY`        | Yes      | apid + meterd                                 |
| `FAAS_PADDLE_WEBHOOK_SECRET` | Yes      | apid (`/v1/webhooks/paddle`)                  |
| `FAAS_PADDLE_SANDBOX`        | Recommended for non-prod (`1` / `true`) | apid + meterd — sandbox host (`api.sandbox.paddle.com`) vs production (`api.paddle.com`) |

The Paddle webhook secret is the shared HMAC-SHA256 key Paddle's
dashboard shows under **Developer tools → Notifications → Endpoints**.
The same key signs all events for that endpoint; the
`Paddle-Signature` header carries `ts=…;h1=…` and
`paddle.Provider.VerifyWebhook` checks both halves with a 5-minute
clock-skew tolerance.

### Polar (opt-in)

| Var | Required | Used by |
|-----|----------|---------|
| `FAAS_POLAR_ACCESS_TOKEN` | Yes | apid + meterd |
| `FAAS_POLAR_WEBHOOK_SECRET` | Yes | apid (`/v1/webhooks/polar`) |
| `FAAS_POLAR_SANDBOX` | Recommended for non-prod (`1` / `true`) | apid + meterd |
| `FAAS_POLAR_HOBBY_PRODUCT_ID` | Yes for the Hobby plan | apid checkout + webhook plan mapping |
| `FAAS_POLAR_PRO_PRODUCT_ID` | Yes for the Pro plan | apid checkout + webhook plan mapping |
| `FAAS_POLAR_SCALE_PRODUCT_ID` | Yes for the Scale plan | apid checkout + webhook plan mapping |
| `FAAS_POLAR_USAGE_EVENT_NAME` | Optional | Defaults to `faas_ram_usage` |
| `FAAS_POLAR_METER_ID` | Yes | Meter used for ingestion and reconciliation; must sum `gb_ram_hours` |
| `FAAS_POLAR_SUCCESS_URL` / `FAAS_POLAR_RETURN_URL` | Optional | Hosted-checkout redirects |
| `FAAS_POLAR_WEBHOOK_TOLERANCE_SECONDS` | Optional | Standard Webhooks timestamp tolerance; defaults to 300 seconds |

Polar products are not auto-created by this provider. In the Polar
dashboard, create one active monthly recurring product per paid plan with:

- one fixed EUR price matching Gregale's monthly plan price (€9, €29, or €99);
- one EUR metered-unit price of `1` cent per unit, backed by the configured
  meter, with no Polar cap; and
- no `meter_credit` benefit. Gregale removes the plan's included allowance
  locally because its quota resets on the UTC calendar month, while Polar
  credits reset on a subscription billing cycle.

The meter must filter the configured event name and sum `gb_ram_hours`. The
meterd pusher sends only the net overage for the account's UTC calendar month;
the event's `mb_seconds` metadata records that same net quantity exactly as an
integer for reconciliation and audit.

Configure a Polar webhook endpoint at
`https://<your-api-host>/v1/webhooks/polar` and subscribe at least to
`subscription.created`, `subscription.updated`, `subscription.active`,
`subscription.canceled`, `subscription.revoked`, `subscription.past_due`,
`order.created`, `order.paid`, and `order.refunded`. Polar signs deliveries with Standard
Webhooks headers (`webhook-id`, `webhook-timestamp`, and
`webhook-signature`); the provider verifies the body before parsing it.

The implementation supports customer creation/reuse, hosted checkout,
provider customer-portal sessions, metered event ingestion, scheduled
cancellation, refunds, and read-only usage reconciliation. Set
`FAAS_POLAR_METER_ID` to the UUID of the meter that sums `gb_ram_hours`; the
provider converts Polar's total back to the local integer `mb_seconds` unit.
The catalog preflight rejects missing or mismatched product prices, meters,
archived resources, and attached Polar meter credits before either daemon
starts.

Polar does not expose a direct saved-card charge retry API, so `faas billing
retry` returns a truthful 501 with the customer-portal URL; customers should
update their payment method and recover the subscription from that portal.

Polar usage events are attributed to billing periods by receipt time, not the
event's supplied timestamp. meterd therefore pushes each completed hour and
rechecks a durable 30-day history on every pass. A Polar configuration whose
push interval is longer than one hour is rejected, so a restart or temporary
Polar outage cannot silently under-report usage.

### Production release gates

Before enabling Polar in production, verify that the three paid-plan product
IDs point to active monthly recurring EUR products containing exactly the
fixed and metered prices described above. Both `apid` and `meterd` fail
startup when a required product, meter, price, or meter wiring check fails.
The deployment must use the hourly `meterd` pass and include migration
`00588`.

Polar's raw `polar_whs_...` webhook secret is accepted alongside the existing
base64/`whsec_` form. Invoice projections are persisted idempotently from
`order.created` and updated by `order.paid`; a persistence failure returns
non-2xx so Polar retries. Run the host smoke test after rendering config:

```sh
sudo deploy/scripts/verify-secrets.sh
```

Official references: [Polar checkout sessions](https://polar.sh/docs/api-reference/checkouts/create-session),
[customer portal sessions](https://polar.sh/docs/api-reference/customer-portal/sessions/create),
[event ingestion](https://polar.sh/docs/api-reference/events/ingest),
[meter quantities](https://polar.sh/docs/api-reference/meters/get-quantities),
[usage meters](https://polar.sh/docs/features/usage-based-billing/meters),
[subscription cancellation](https://polar.sh/docs/features/subscriptions/manage),
and [Standard Webhooks delivery](https://polar.sh/docs/integrate/webhooks/delivery).

## Cutover procedure (Stripe → Paddle)

1. **Inventory the existing customer mappings.** Stripe
   `cus_…` values live in `accounts.provider_customer_id`. Paddle uses
   `ctm_…`. The column is **reused** for both providers per ADR-032
   (the rename to `provider_customer_id` is a separate follow-up PR).
   New Paddle customers get fresh `ctm_…` IDs on first checkout; there
   is no in-place cus→ctm migration.

2. **Provision the Paddle credentials.**
   - Generate a Paddle API key (sandbox or live).
   - Create a webhook endpoint in the Paddle dashboard pointing at
     `https://apid.gregale.dev/v1/webhooks/paddle`. Copy the webhook secret.

3. **Stand up the new billing surface in parallel.** The two providers
   can coexist in different environments — do not point the production
   apid at Paddle until a small set of test customers has completed
   first-time checkout + a paid upgrade + a payment-failed recovery
   on the sandbox.

4. **Set the env vars on apid + meterd.** Restart both daemons. Boot
   logs include the line `billing provider loaded provider=paddle` —
   absence of this line means the env var didn't reach the daemon
   (systemd drop-in mis-config, container env filter, etc.).

5. **Watch the dunning state machine for one full cycle.** A redelivered
   Paddle event should land in `journalctl -u faas-apid` as a 200
   with no side effect; a `transaction.payment_failed` flips the
   seeded account to `past_due`; a `transaction.paid` flips it back
   to `active`.

## Cutover procedure (Paddle → Stripe)

Unset `FAAS_BILLING_PROVIDER` and redeploy. Stripe is the default, so
the absence of the var is sufficient. The Stripe path is bit-for-bit
unchanged from pre-PR-#3 — the apid changePlan 402 returns
`billing_portal_url` with the `{account_id}` substitution, the
`/v1/webhooks/paddle` mount returns 503 if a request does land there
(provider not configured), and meterd pusher loop dispatches through
the legacy `stripe.Client`.

## Failure modes

| Symptom                                                    | Likely cause                                                                  | Diagnostic                                                                                          |
|------------------------------------------------------------|-------------------------------------------------------------------------------|-----------------------------------------------------------------------------------------------------|
| Boot fails with `unknown FAAS_BILLING_PROVIDER`            | Typo in the env var (e.g. `braintree`, `paypal`). Set to `paddle` or unset.  | Boot log carries the typed error.                                                                   |
| Boot fails with `paddle EnsurePlanProducts: …`             | Network egress to `api.paddle.com` blocked (or `api.sandbox.paddle.com` if sandbox=1). Check `iptables` + `FAAS_BRIDGE_OUTBOUND`. | Boot log line; idempotent — re-run after fixing.                                                    |
| Webhook returns 503                                        | `FAAS_PADDLE_WEBHOOK_SECRET` is empty. Provider refuses to verify.            | Boot log `paddle_webhook.no_provider`.                                                              |
| Webhook returns 400 (`code: validation_failed`)            | Signature mismatch (wrong secret in dashboard) or clock skew > tolerance.     | `journalctl -u faas-apid` shows `paddle_webhook.verify_failed err=…`. Also increments `paddle_webhook_verify_failed_total`. |
| `transaction.paid` 503 or no state flip                   | Unknown customer (event's `data.customer_id` doesn't match `accounts.provider_customer_id`) or unavailable replay/state storage. 503 keeps the delivery retryable. | `journalctl -u faas-apid` shows `paddle_webhook.unknown_customer` or `paddle webhook replay/state …`. Check the mapping and database. |
| `changePlan` 402 carries `paddle_checkout_url` but URL 404 | Paddle sandbox product/price IDs not yet created. Run `EnsurePlanProducts` manually (it's idempotent — re-running on an existing catalog is a no-op). | `faas billing price-catalog list` shows the catalog snapshot.                                       |
| Duplicate Paddle events arriving within seconds            | Paddle redelivery (network blip). Deduped by `pkg/webhookdedupe` (5-min TTL). | `paddle_webhook_replay_suppressed_total` increments; audit row `webhook.replay_rejected` is emitted. |
| Polar usage is missing after a restart/outage              | Stale meterd config or unapplied migrations.                                | Confirm hourly meterd cadence, migration `00588`, and `failed` entries from meterd `/healthz`. |
| Polar webhook returns 503                                   | Invoice/replay persistence is unavailable.                                  | Fix Postgres or migrations; Polar retries the non-2xx delivery. |
| Polar invoice history is empty                              | `order.created` is not subscribed or the customer/product IDs do not map.   | Check Polar webhook subscriptions and `polar_webhook.unknown_customer` logs. |

## Webhook hardening knobs

| Env var                                   | Default | Purpose                                                                                  |
|-------------------------------------------|---------|------------------------------------------------------------------------------------------|
| `FAAS_PADDLE_WEBHOOK_TOLERANCE_SECONDS`   | `300`   | Replay-protection window. Applies symmetrically (rejects future-dated too). The default matches Stripe. Useful when sandbox VMs have bad NTP. |
| `FAAS_POLAR_WEBHOOK_TOLERANCE_SECONDS`    | `300`   | Standard Webhooks timestamp window for Polar. Applies symmetrically and rejects future-dated deliveries. |

Prometheus counters (single-registry on every daemon; only `apid` increments):

- `paddle_webhook_verify_failed_total` — unlabelled; tripwire for "wrong webhook secret in dashboard" or "clock skew beyond tolerance".
- `paddle_webhook_replay_suppressed_total` — unlabelled; tripwire for "Paddle is redelivering" (sustained rate is normal during a Paddle-side incident; sustained rate over 30 min is the alert).

## Sandbox walk

`make e2e-sandbox` exercises the full `changePlan → 402 → webhook → state-flip` round-trip against `api.sandbox.paddle.com`. Operator-only — gated on `secrets/.env.sandbox` + `FAAS_PADDLE_SANDBOX_E2E=1`. Skipped in CI by design (no Paddle sandbox account in CI secrets).

```sh
# 1. Create secrets/.env.sandbox with the two keys from
#    https://sandbox-vendors.paddle.com → Developer tools → Authentication.
cat > secrets/.env.sandbox <<EOF
FAAS_PADDLE_SANDBOX_API_KEY=pdl_sandbox_…
FAAS_PADDLE_SANDBOX_WEBHOOK_SECRET=whk_…
EOF
chmod 0600 secrets/.env.sandbox

# 2. Set DATABASE_URL for the harness (any reachable Postgres).
export DATABASE_URL=postgres:///faas?host=/run/postgresql&user=faas

# 3. Run the walk.
make e2e-sandbox
```

The walk fires five sequential tests via a `/tmp/faas-paddle-sandbox-handoff.json` file:

1. `TestPaddleSandbox_ChangePlanReturnsCheckoutURL` — signup → `PATCH /v1/account/plan {plan: hobby}` → asserts 402 + `paddle_checkout_url` + `tx_id`. Writes the customer + transaction IDs to the handoff file.
2. `TestPaddleSandbox_SubscriptionCreatedStampsCustomerID` — POSTs a signed `subscription.created` event with the handoff's `ctm_…` ID → asserts `accounts.provider_customer_id` is populated.
3. `TestPaddleSandbox_TransactionCompletedIsNoop` — POSTs a signed `transaction.completed` event → asserts no state flip.
4. `TestPaddleSandbox_PerWindowClaimRoundTrip` — runs the meterd production path (`NewProviderWithDedupe` → `EnsurePlanProducts` → `PushUsageRecord`) directly against the sandbox and asserts the `paddle_overage_dedupe` row is stamped with `state=completed`, `pushed_at` non-null, `claimed_by` non-null, `pushed_mb_seconds` = the integer the test pushed. Distinct from `pkg/billing/paddle/sandbox_test.go` (which exercises the Provider with `dedupe=nil`); this one wires the production constructor and asserts the dedupe row state after a real SDK POST.
5. `TestPaddleSandbox_WebhookSignatureRoundTrip` — SDK-side pinning of the contract Tests 2/3 prove at the apid HTTP layer. Signs a real Paddle-shaped JSON body with the operator's webhook secret; asserts `VerifyWebhook` accepts it with canonical and lowercase header keys, and rejects a tampered body with `errors.Is(err, billing.ErrBadSignature)`.

A passing run validates that the apid webhook handler, the catalog OpProvider, the meterd pusher, and the live Paddle sandbox all agree on the customer → account mapping AND on the wire shape the SDK signs/verifies. Re-run is idempotent (every test creates fresh state).

The B4 pre-flight has a separate pgstore-level pin in `pkg/state/pgstore_paddle_overage_schema_test.go` that runs in CI's pg shard (no sandbox credentials required). The two probes (`TestPgStorePaddleOverageDedupeSchema_PostApply` + `_PreApply_ReturnsTableMissing`) keep the to_regclass + information_schema probe honest — a future migration that drops a 00041 column or breaks the missing-table hint flips them red.

## Secret rotation

Paddle API keys and webhook secrets are configured through the `sealed.env` systemd `EnvironmentFile=` (`/etc/faas/sealed.env`), not through on-disk secret files. The TOML equivalent (`[billing.paddle]` in `apid.toml` / `meterd.toml`) covers the same fields for containerized deploys; the loader's `ApplyBillingEnvOverlay` makes **env win over TOML** when both are set (`pkg/billing/loader/config.go:157-172`). Rotation cadence is monthly per `docs/ops/secrets-rotation.md`.

Procedure (env form):

1. Generate the new key/secret in the Paddle dashboard (Developer tools → Authentication, Developer tools → Notifications).
2. Edit `/etc/faas/sealed.env` — replace `FAAS_PADDLE_API_KEY` and/or `FAAS_PADDLE_WEBHOOK_SECRET`. The file is mode `0600 root:root`; do not `chmod` it.
3. `systemctl restart faas-apid faas-meterd` — both daemons read the env vars at boot; mid-flight rotation requires a process restart.
4. Validate: `faas billing status` prints the new catalog SyncedAt timestamp (re-stamped on `EnsurePlanProducts` at boot). Send a Paddle test event from the dashboard (Notifications → your endpoint → Send test) and confirm `journalctl -u faas-apid` shows `paddle_webhook` activity without `verify_failed`.

Procedure (TOML form — for containers / k8s):

1. Generate the new key/secret in the Paddle dashboard.
2. Update the `[billing.paddle]` block in `apid.toml` / `meterd.toml` and redeploy. The `Secrets:` secret-store abstraction (k8s `Secret`, docker `--env-file`, etc.) is the operator's choice.
3. Roll the daemons.

For gap-G2 sealing at rest (sensitive providers), wrap the `sealed.env` write in the operator's age / sops / vault flow — the repo's `host.age` precedent (`docs/ops/host-age-rotation.md`) is the canonical pattern.

## Health checks

- `/v1/webhooks/paddle` should respond 503 (provider not configured) on
  a Stripe-default box — a 200 here is a bug.
- The `apid` boot log line `billing provider loaded provider=paddle`
  is the canonical "the env var reached the daemon" signal.
- `meterd` boot log: `meterd billing provider loaded provider=paddle`.
- After PR-P4 (this PR): `make verify-secrets` fails the playbook if
  `FAAS_BILLING_PROVIDER=paddle` is set without `FAAS_PADDLE_API_KEY`.
- Operator-side smoke test: `make doctor-paddle` (operator-only target,
  not in CI) prints `faas billing status --watch` output + the last
  60 seconds of `faas-apid` journal lines.

## Column rename history (note)

The rename `accounts.stripe_customer_id → accounts.provider_customer_id` already shipped in migration **00040** (well before PR-P4). Both providers write to the same column: `stripe.Customer.ID` → `cus_…`, `paddle.Customer.ID` → `ctm_…`. For an operator querying the table:

```sql
SELECT provider_customer_id FROM accounts WHERE id = '…';
```

The column has been provider-neutral since 00040; PR-P4 added **no** rename migration. The earlier plan for PR-P4 included the rename as Stream F, but the work was already done upstream and a re-rename would have been a no-op guarded by the DO-block — the migration was removed before commit. ADR-032 §Consequences records the original deferral and its subsequent resolution.

## Related

- ADR-032 — Paddle as an opt-in billing provider (decision record; PR-split section amended by PR-P4 to reflect that the column rename was already shipped in 00040).
- ADR-025 — provider-pluggable billing layer (the abstraction).
- ADR-042 — webhook replay protection (the `pkg/webhookdedupe` contract).
- `pkg/billing/loader/` — the canonical selector implementation.
- `pkg/billing/paddle/` — the Paddle Billing v2 implementation.
- `docs/runbooks/BillingDrift.md` — alert runbook for `meterd_billing_drift_*` (works for both providers).

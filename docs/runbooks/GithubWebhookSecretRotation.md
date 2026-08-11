# GithubWebhookSecretRotation

Source: PR-D / ADR-012 §7 amendment
(`docs/adr/012-githubd.md` §7).

When the per-tenant GitHub App webhook secret for one
`installation_id` needs to rotate. Three trigger paths:

1. **Operator-driven rotation.** A tenant requests a rotation
   through the dashboard / CLI; the operator runs
   `gregale github-webhook-secret set` and updates the GitHub
   App setting on github.com (the two sides of the rotation).
2. **Leaked secret.** The tenant's webhook secret leaked (CI
   log, browser extension, exfil). Treat as a §11 incident:
   rotate immediately and audit `githubd.webhook_secret_set`
   events.
3. **Routine key hygiene.** The tenant is on the §11 90-day
   rotation cadence (same shape as the Faas host age key).

## Symptom / detect

The Prometheus counter
`githubd_webhook_secret_total{status="set"}` is emitted by the
apid admin route on every successful rotation; the rate should
track the expected rotation cadence. An unexpected spike is a
leak signal — page on `rate > 0.5/h` for 1h.

The fail-closed counter
`githubd_webhook_secret_total{status="db_error"}` is emitted by
`pkg/githubd/webhook_secret.go::Resolve` when the per-tenant
DB read fails. Page after 5m of non-zero rate — a partial DB
outage is silently rejecting webhooks for installs that
*migrated* off the platform secret.

## Rotate (operator path)

```sh
# 1. Generate a new secret. 32 bytes is the GitHub recommendation;
#    the server accepts 16..64 bytes.
SECRET=$(openssl rand -hex 32)

# 2. Write the per-tenant row (admin-scoped API key required).
echo -n "$SECRET" | gregale github-webhook-secret set \
    --installation-id <INSTALLATION_ID> \
    --from-stdin

# 3. Set the new secret on the GitHub App side (the customer does
#    this; the operator's job is just the per-tenant row + the
#    audit row).

# 4. Verify the resolver picks up the new value. The resolver
#    invalidates the cache on every `set` call, so the next
#    inbound webhook uses the new row.
```

## Fall back to the platform secret (rollback)

The per-tenant row is additive; deleting it falls back to
`FAAS_GITHUB_WEBHOOK_SECRET` for that install. If the tenant
cannot complete the rotation, drop the row so the install is
not stranded on a stale value:

```sql
DELETE FROM github_webhook_secrets
WHERE installation_id = <INSTALLATION_ID>;
```

The resolver cache invalidates within 60s (TTL). The next
inbound webhook uses the platform secret.

## Audit

The `githubd.webhook_secret_set` audit event carries the
operator's `account_id` + `installation_id`. Cross-reference
with the GitHub App's webhook delivery log to confirm the
secret on github.com was actually rotated.

## Related

- ADR-012 §7 — the per-tenant secret decision + rationale.
- `pkg/githubd/webhook_secret.go` — the resolver implementation.
- `pkg/githubd/webhook.go::VerifyPushSignature` — the verifier
  (unchanged; the resolver only swaps the secret bytes).
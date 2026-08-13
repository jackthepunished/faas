# PreviewSubdomainRouting

Source: `cmd/gatewayd-internal/backend.go::pgRouter.ResolveHost`,
`pkg/gateway/allowlist.go::NewPGAllowlist`,
`pkg/gateway/preview_parser.go::PreviewScopeFromHost`,
`migrations/00220_preview_app_columns.sql` (PR-A spine).
Metric: customer-reported via support tickets (no Prometheus signal
yet — preview routing is per-customer and per-PR, not a fleet
aggregate). Spec: §4.1.2.8b + ADR-095 PR-B.
Severity: warn.

> **Preview environments (issue #272 / ADR-095).** PR-A shipped the
> webhook spine (preview app row, source-ref deploy, Check Run);
> PR-B shipped the routing slice (subdomain parser +
> cert-allowlist extension); PR-C ships the teardown janitor. This
> runbook covers the PR-B surface. The prod URL
> (`{slug}.apps.<zone>`) keeps working — only preview-shaped
> hostnames (`pr-{N}.{slug}.apps.<zone>`) are affected.

## Symptom

A customer reports one of:

- `https://pr-42-myapp.apps.gregale.dev/` returns **404** (routing
  layer can't resolve the hostname to an apps row).
- `https://pr-42-myapp.apps.gregale.dev/` returns **503 / NoLiveDeployment**
  (apps row found, but no deployment with `scope='pr-42'` has reached
  the WARM/RUNNING state yet).
- The hostname hangs at the TLS handshake — `curl -v` shows
  "certificate verify failed" or "no alternative certificate subject
  name matches" — even though `*.apps.<zone>` is healthy.

The prod URL for the same app (`myapp.apps.gregale.dev`) keeps
working in all three cases — the preview path is isolated from
prod by the per-row `preview_pr_state` gate.

## Triage by cause

### Routing layer returns 404

The parser in `pkg/gateway/preview_parser.go::PreviewScopeFromHost`
rejects the hostname shape. Verify the locked contract:

| host | result |
|---|---|
| `pr-42-myapp.apps.gregale.dev` | (42, "myapp", true) ✓ |
| `PR-42-myapp.apps.gregale.dev` | (0, "", false) — case-sensitive |
| `pr-0-myapp.apps.gregale.dev` | (0, "", false) — PR number 0 refused |
| `pr-42.apps.gregale.dev` | (0, "", false) — missing slug |
| `pr-abc-myapp.apps.gregale.dev` | (0, "", false) — non-numeric |
| `pr-007-myapp.apps.gregale.dev` | (0, "", false) — leading zero refused |
| `pr-42-foo.bar.apps.gregale.dev` | (0, "", false) — inner dot |

If the parser returns ok=true but the router still 404s, the
preview `apps` row is missing. Check:

```bash
psql -U faas -d faas -c \
  "SELECT id, slug, preview_of_slug, preview_pr_state, preview_expires_at \
   FROM apps WHERE slug='pr-42-myapp';"
```

If the row is missing entirely, the webhook never fired (or was
refused for fork PR per ADR-094 D3). Cross-reference the GitHub
Check Run `name="gregale-preview"` on the PR — conclusion
`neutral` with text "Preview skipped for fork PR" means D3 refused.
Otherwise the row should exist within ~30s of `pull_request.opened`.

If the row exists but `preview_pr_state='torn_down'` or
`'stale'`, the teardown janitor already ran (PR-C slice). The
preview app is gone for good — a fresh push to the PR rebuilds
it under the same slug (post-teardown the slug is reusable).

### Routing layer returns 503 / NoLiveDeployment

The apps row exists with `preview_pr_state='open'` but no
RUNNING/WARM instance is keyed to `scope='pr-42'`. Most common
cause: the source-ref deploy (PR-A's enqueue path) is still
running. Check:

```bash
psql -U faas -d faas -c \
  "SELECT id, state, scope, error FROM builds \
   WHERE app_id=(SELECT id FROM apps WHERE slug='pr-42-myapp') \
   ORDER BY created_at DESC LIMIT 5;"
```

Cold-boot budget for a preview app is identical to prod (~350 ms
warm / up to 30s cold). If `state` is still `building` or `queued`
after 60s, the build slot is starved — see `FaasBuildQueueBacklog`
for the prod variant.

If `state='failed'`, the build error is in the `error` column.
The preview Check Run on GitHub carries the same error verbatim.

### Cert handshake fails

The wildcard `*.apps.<zone>` is single-label RFC 2818 — it does
**not** match `pr-{N}.{slug}.apps.<zone>` (two-deep). Per-host
on-demand HTTP-01 mint fires through certmagic. The allowlist in
`pkg/gateway/allowlist.go::NewPGAllowlist` has two branches:

1. **Custom-domain** — `custom_domains.verified_at IS NOT NULL`.
2. **Preview** — `apps.preview_pr_state='open'` for the row whose
   slug is `pr-{N}-{parent-slug}`.

If the cert was minted but is now stale (the preview app moved to
`closed`/`stale`/`torn_down`), gatewayd-internal still serves it
until the renew loop evicts — see
`FaasTLSCertExpiryPageByHost.md` for the cert lifecycle. The
allowlist itself is purely an issuance gate.

To confirm the allowlist accepted the host:

```bash
journalctl -u faas-gatewayd-public \
  | grep -E 'allowlist preview|on-demand denied'
```

`on-demand denied` for `pr-42-myapp.apps.gregale.dev` with no
matching allowlist row means one of: (a) the apps row is missing
or `preview_pr_state != 'open'`; (b) `appsSuffix` in the gatewayd
config doesn't match the hostname's suffix (the prefix `appsSuffix`
gate runs first); (c) Postgres is down and the allowlist is
fail-closed (loud `allowlist lookup failed` Warn line).

## Verify

```bash
# 1) Is the parser accepting the customer's hostname?
go test ./cmd/gatewayd-internal/ -run TestPreviewScopeFromHost -v -count=1

# 2) Is the allowlist accepting the same hostname? (uses the same parser)
go test ./pkg/gateway/ -run TestOnDemandAllowlist_PreviewHost -v -count=1

# 3) Is the routing + allowlist shared-store invariant intact?
go test ./pkg/gateway/ -run TestRoutingAndCertAllowlistShareStore -v -count=1

# 4) Is the production row correct?
psql -U faas -d faas -c \
  "SELECT id, slug, preview_of_slug, preview_pr_number, preview_pr_state \
   FROM apps WHERE slug='pr-42-myapp' AND preview_pr_state='open';"
```

All four checks green ⇒ the preview is wired; an in-flight wake is
the remaining cause. A non-green check pinpoints which seam
broke (parser, allowlist, store, or production row).

## Recover

For routing 404:

```bash
# Force a rebuild from the source-ref path if the row is missing:
# (the webhook normally re-fires on next push; manual rebuild is
#  only needed when the original webhook was refused or eaten by
#  an at-capacity DeployedAppMax.)
faas apps rebuild --slug=myapp --pr=42
```

For cert denial:

```bash
# Trigger a fresh mint by hitting the host directly. Certmagic's
# allowlist is what matters; the renew loop will retry until
# NewPGAllowlist returns true.
curl -v https://pr-42-myapp.apps.gregale.dev/healthz

# If the cert is stuck (allowlist now allows but certmagic
# hasn't re-tried), restart the daemon to flush the in-process
# mutex that serializes on-demand mints per hostname:
systemctl restart faas-gatewayd-public
```

For build starvation:

```bash
# Inspect the build queue; see FaasBuildQueueBacklog for the full
# recovery recipe.
psql -U faas -d faas -c \
  "SELECT count(*) FROM builds WHERE state IN ('queued','building');"
```

If the customer's `DeployedAppMax` (Free=1, Hobby=5, Pro=25,
Scale=100) is exhausted, the webhook returns `429
deployed_app_capacity` and the Check Run carries the upgrade hint.
This is the documented behaviour per ADR-094 D4 — previews share
the same quota as prod. Help the customer free a slot or upgrade
their plan.

## Silence

Preview-host routing issues are per-customer and per-PR, not
fleet-wide. There is no fleet alert to silence. Track the
customer ticket with the linked preview URL and the affected
`apps.id` from `Verify` step 4.

For recurring build starvation during high-PR windows (a customer
with 20+ open PRs on Pro), coordinate with the customer to either
close stale PRs (the teardown janitor will reap them within 24h
of PR close per ADR-095 PR-C) or batch their work into fewer
long-lived branches.

## Notes

The cert-mint surface for previews deliberately reuses the
production `NewPGAllowlist` rather than a parallel allowlist table
— there is one source of truth for "hostname the edge is willing
to mint a cert for" and it lives in `pkg/gateway/allowlist.go`.
The preview branch's `previewOpen()` assertion (`apps.preview_pr_state
== 'open'`) is the only check; the row's `preview_of_slug` must
equal the parent slug extracted by `PreviewScopeFromHost` (the
parser enforces this via the apps-suffix gate — a hostname like
`pr-42-foo.apps.gregale.dev` resolves to the row with slug
`pr-42-foo`, which the webhook provisions with `preview_of_slug='foo'`
per ADR-094).

The TTL deadline for a stale preview is `preview_expires_at`
(default `now() + 7 days` per `pkg/githubd/service.go`). Once the
janitor transitions the row to `torn_down`, the slug is reusable
for a future PR — the row gets tombstoned (`apps.deleted_at`),
not deleted.

Cache invalidation between the routing layer and the allowlist is
purely row-level: `apps_update` notifications (`pg_notify`) flush
the per-host route cache in `pkg/gateway/pgbackend.go` so a
`preview_pr_state` flip from `open` → `closed` is visible to
both within ~1s of the change.

# ADR-091: Operator observability backend (`/v1/admin/obs/*`)

- Status: Accepted (PR-1 of 3)
- Date: 2026-08-10
- Scope: `apid` HTTP surface, `pkg/api/limits.go`, `cmd/apid/server.go`,
  `cmd/sdk-coverage/main.go`, `cmd/apid/spec_compliance_test.go`,
  `migrations/00174_admin_obs_index.sql`.

## 1. Context

The platform owner has zero visibility into the running system: no way to
answer "do I have any users", "who is on what plan", "are nodes healthy",
or "what just happened". The customer dashboard at `/dashboard/*` is
per-account only. A second agent will build the operator web frontend;
this ADR pins the backend that frontend talks to and the cross-cutting
decisions that the three PRs need to keep consistent.

The two-layer admin gate (`api.ScopesAdminOnly` scope + `s.adminAllows`
email allowlist, fed by `FAAS_ADMIN_EMAILS`) already exists in `apid`.
We extend it rather than create a new auth surface, so the SOC 2
evidence trail keeps one allowlist.

The full `/v1/admin/obs/*` suite is too large for one PR (cross-tenant
data-leak surface, three new sqlc aggregates, ADR-sized scope). We ship
it in three PRs; this ADR pins the architectural decisions so PRs #2 and
#3 don't reopen them.

## 2. Scope

### PR #1 — Visibility MVP (this PR)

- `GET /v1/admin/obs/overview` — KPI bundle (active accounts, live
  instances, node health summary, top-5 rate-limited accounts in last
  24h, recent failure counts in last 1h).
- `GET /v1/admin/obs/tenants` — paginated list. PII redacted by default;
  `?include_pii=1` opts in and emits a `pii.accessed` audit row.
- `GET /v1/admin/obs/tenants/{id}` — per-tenant drill-down (account,
  apps, orgs, api-key/session counts). 404 on unknown id.
- `GET /v1/admin/obs/nodes` — compute-node list (alias of
  `/v1/compute-nodes`, filtered to admin projection).
- `GET /v1/admin/obs/nodes/{name}/heartbeats` — per-node heartbeat
  window (default 30m, hard cap 24h, default 200, cap 2000).

### PR #2 — Anomalies + rate-limits

- `GET /v1/admin/obs/anomalies` — traffic anomalies from `usage_minutes`.
- `GET /v1/admin/obs/rate-limits` — per-account rate-limit aggregate
  (durable view from `events` + live snapshot from
  `authLimiter.Snapshot()`).
- New sqlc: `PerAccountRateLimitAggregate`, `TrafficAnomalyAggregate`.

### PR #3 — Audit + events + SSE

- `GET /v1/admin/obs/audit-log/search` — filter on the FK-free
  `audit_log` table (`account_id`, `kind_prefix`, free-text on `data`,
  `include_anonymous`, `since`).
- `GET /v1/admin/obs/events` — live `events` table read (distinct from
  audit-log search; documented boundary).
- `GET /v1/admin/obs/nodes/events` — SSE stream (mirrors the existing
  `cmd/apid/compute_nodes_events.go`; the older path gets a
  `Deprecation` header in this PR).
- New sqlc: `ListAllEventsPaged`, `ListRecentEventsForAccount`.

## 3. Decisions

### 3.1 Two-layer auth gate (no new surface)

`api.ScopesAdminOnly` scope + `s.adminAllows(acct)` email allowlist. No
new scope, no new allowlist, no new env var. The same
`FAAS_ADMIN_EMAILS` allowlist that gates `/v1/admin/billing-paddle-*`
and the legacy `/v1/compute-nodes` surfaces the operator only at the
gateway. One allowlist → one SOC 2 evidence trail.

The two layers compose: a key with the admin scope reaches the handler
only if the owning account's email is in `FAAS_ADMIN_EMAILS`. A
non-admin scope is rejected at the middleware layer (403 with code
`admin_required`) before the handler body executes. A custom scope +
non-allowlist email is rejected inside the handler with the same
`admin_required` code (defense-in-depth — a future middleware bypass
cannot surface data).

### 3.2 MFA required (no step-up)

Every `/v1/admin/obs/*` read requires MFA. This contradicts the
precedent set by `/v1/admin/billing-paddle-catalog` (no MFA) and
intentionally does so: the obs surface reads secrets-adjacent
metadata (org_slug → tenant identity, deployment counts → business
state). A compromised admin API key without MFA on the operator's
machine should not silently grant access to the tenant list.

Step-up is intentionally rejected: the cookie session enforces MFA at
cookie-mint; programmatic callers re-authenticate. A "high-value"
first-read that downgrades to no-MFA later is a foot-gun.

### 3.3 PII redaction by default

`?include_pii=1` is the only opt-in. The default tenants list/detail
response carries the email field as the empty string. The wire DTOs
use `omitempty` so the field is absent from the body, not just blank.

Every `?include_pii=1` call emits an audit row of kind `pii.accessed`
with `data={endpoint, account_id}`. PR #1 ships the emission helper
(`emitPIIAccessed`) but the row itself is wired in PR #3 once the
audit-log search endpoint is in place and the operator can verify the
emit against the audit-log reader. Until then, `?include_pii=1` is
operational but not auditable — this is the documented gap. PR #3
flips the gap closed.

The grep tests in `handlers_admin_obs_security_test.go` pin the
default-redact posture against accidental regressions
(`TestObsSecurity_NoPIIOnDefaultTenantsList` etc.).

### 3.4 Audit-log search reads `audit_log`, not `events`

The two are distinct retention semantics: `audit_log` (migration
00163) is the immutable record the customer-facing
`/v1/audit-log/all` already exposes; `events` is the live operational
feed with shorter retention and higher write rate. PR #3 ships
`/v1/admin/obs/audit-log/search` against `audit_log` and
`/v1/admin/obs/events` against `events`. The two endpoints share the
query shape (cursor, `since`, `kind_prefix`, free-text) but not the
table.

### 3.5 Rate-limit endpoint: durable + live, with documented lag

`/v1/admin/obs/rate-limits` combines two sources:

1. **Durable**: rows from `events` where `kind='auth.rate_limited'`,
   aggregated by `account_id` over a configurable window. Lag = the
   `pg_notify` round-trip plus the 30s aggregator flush.
2. **Live**: `authLimiter.Snapshot()` in-process view of currently
   rate-limited callers (per-IP, per-account). Live by definition;
   disappears on apid restart.

The response carries a `sources: ["durable", "live"]` field so the
operator UI can render the lag in a tooltip. The 30s lag is
documented inline at the top of the wire response.

### 3.6 Anomalies read `usage_minutes`, not Prometheus

One-box posture: `usage_minutes` (the customer-billing aggregate) is
the cheapest source for the operator view — same table, no Prometheus
dependency, no second control-plane process. The aggregate lives in
SQL (a sqlc query): per-app and per-account, day-over-day delta, Z-
score > 3.0 in a 7-day window.

When the control plane goes multi-host (per §"Scale-out"), this
endpoint moves to a PromQL evaluation in the metrics daemon. The
single-binary `apid` cannot speak PromQL today and inventing one in
PR #2 would be over-engineering.

### 3.7 Pagination caps are global constants

Operator UI differs from customer UI:

| Surface                | Default | Max  | Window max |
|------------------------|---------|------|------------|
| `/v1/audit-log`        | 25      | 100  | —          |
| `/v1/admin/obs/*`      | 200     | 500  | 168 h      |

Encoded in `pkg/api/limits.go` as `ObsAdminPaginationDefault`,
`ObsAdminPaginationMax`, `ObsAdminWindowMaxHours`. Inline literals in
handler bodies fail the lint gate (CLAUDE.md "every quota/limit
lives in `pkg/api/limits.go`"). Over-cap limit returns 400 with
`WithLimit(maxN, observed)` per `pkg/api/paging.go:63` — a misconfigured
operator client gets an actionable response, not a silent cap.

### 3.8 Route exclusions: two lists in lockstep

All `/v1/admin/obs/*` routes are excluded from the public OpenAPI SDK
surface (operator-only, mirrors the existing `/v1/compute-nodes`
exclusion). The two `routeExclude` lists —
`cmd/apid/spec_compliance_test.go` and `cmd/sdk-coverage/main.go` —
are updated in the same commit. The PR description must call out
"two-list sync" as a checklist item; reviewers must verify both
files changed.

### 3.9 `/v1/compute-nodes*` and `/v1/admin/obs/nodes*` co-exist

The two read the same underlying tables; the only difference is the
projection (obs adds PII-safe omits, never re-derives `target_url`).
For one release both ship in parallel; PR #3 adds a `Deprecation`
header to `/v1/compute-nodes*` and the routes 410 Gone in a later
cleanup PR. The dual-ship window is the migration path for any
existing dashboard consumers.

## 4. Sensitive fields (never exposed)

Projection helpers in `cmd/apid/handlers_admin_obs_projection.go`
explicitly omit:

- `accounts.mfa_secret_encrypted`, `mfa_recovery_codes_hash`
- `account_passwords.hash`
- `api_keys.key_sha256` (fingerprint only — not on the wire today)
- `sessions.binding_hash`, `sessions.issued_ip`
- `login_tokens.token_hash`, `cli_auth_codes.token_hash`,
  `org_invitations.token_hash`
- `app_secrets.ciphertext`, `app_envs.value`
- `app_registry_credentials.password_encrypted`
- `app_webhooks.webhook_secret_sealed`,
  `alert_rules.webhook_secret_sealed`
- `instances.netns`, `guest_uid`, `host_ip`, `lease_token`
- `invoices.raw` (provider payload)
- `accounts.email`, `orgs.provider_customer_id` — only via
  `?include_pii=1` with audit row emission
- `oauth_links.email`, `gdpr_requests.account_email` (audit row only)

Grep tests in `handlers_admin_obs_security_test.go` pin every omission
by asserting the absence of well-known column-name markers
(`mfa_secret_encrypted`, `netns`, `guest_uid`, …) on the wire body.
Adding a new sealed column to `state.Account` MUST add a marker to
the grep list and the test will fail until the projection helper
learns to omit the new column.

## 5. SQL — `migrations/00174_admin_obs_index.sql`

```sql
-- +goose Up
CREATE INDEX IF NOT EXISTS orgs_created_at_idx
    ON orgs USING btree (created_at DESC);
CREATE INDEX IF NOT EXISTS orgs_status_idx
    ON orgs USING btree (status) WHERE status <> 'active';
CREATE INDEX IF NOT EXISTS events_kind_at_idx
    ON events USING btree (kind, at DESC);

-- +goose Down
DROP INDEX IF EXISTS events_kind_at_idx;
DROP INDEX IF EXISTS public.events_kind_at_idx;
DROP INDEX IF EXISTS public.orgs_status_idx;
DROP INDEX IF EXISTS public.orgs_created_at_idx;
```

The three partial/composite indexes back the fleet-wide scans that
PR #1 (`orgs_created_at_idx`, `orgs_status_idx`) and
PR #3 (`events_kind_at_idx`) introduce. They are added in PR #1
to keep the migration slot contiguous; the PRs that read them land
later. The original PR-1 plan called for a fourth index
(`builds_account_created_idx`) but the `builds` table only carries
`deployment_id` (per 00001_init.sql) — accounts are reached via
deployments → apps → accounts. PR #2 adds the right shape
(`build_provenance.build_id` + `started_at`) once the build-status
endpoint (issue-741, PR #792) lands.

Replay-safe (`IF NOT EXISTS`); drop in `+goose Down` so the migration
can be reverted in a development database. Migration test asserts the
indexes exist via `pg_indexes` and that EXPLAIN on the rate-limit /
anomaly aggregates uses the new indexes (same shape as
`00165_build_provenance_framework_version_test.go`).

## 6. Wire contract (PR #1)

Times are RFC 3339. IDs are UUID strings. All errors are RFC 7807 via
`api.WriteProblem`. Every list uses `items` + `next_cursor`, always-
non-nil slice, default limit 200, hard cap 500.

### `GET /v1/admin/obs/overview`

```json
200 OK
{
  "generated_at": "2026-08-10T12:00:00Z",
  "totals": {
    "accounts_active": 1234,
    "accounts_past_due": 7,
    "accounts_suspended": 2,
    "orgs_total": 1100,
    "apps_total": 4500,
    "instances_live": 312,
    "instances_waking": 9,
    "nodes_active": 4,
    "nodes_inactive": 1,
    "audit_events_24h": 6789
  },
  "top_rate_limited_accounts_24h": [
    {"account_id": "uuid", "hits": 412}
  ],
  "node_health": [
    {"name": "default-local", "active": true, "last_heartbeat_at": "2026-08-10T11:59:58Z", "stale": false}
  ],
  "recent_failures_1h": [
    {"kind": "deployment.failed", "count": 3}
  ]
}
```

### `GET /v1/admin/obs/tenants`

Query: `?cursor=<opaque>&limit=<n>&plan=<free|hobby|pro|scale>&status=<active|past_due|suspended>&include_pii=<0|1>`

```json
200 OK
{
  "items": [
    {
      "account_id": "uuid",
      "plan": "pro",
      "status": "active",
      "org_slug": "acme",
      "is_personal": true,
      "created_at": "2025-11-04T08:32:11Z",
      "mfa_enrolled": true,
      "apps_count": 12,
      "deployments_live_count": 12,
      "email": "ops@acme.com"
    }
  ],
  "next_cursor": "eyJ...",
  "limit": 200
}
```

### `GET /v1/admin/obs/tenants/{id}`

```json
200 OK
{
  "account": {"account_id":"uuid","plan":"pro","status":"active","org_slug":"acme","is_personal":true,"created_at":"...","mfa_enrolled":true,"email":"ops@acme.com"},
  "apps": [{"id":"uuid","slug":"api","status":"active","deployments":4}],
  "orgs": [{"id":"uuid","slug":"acme","role":"owner"}],
  "api_keys": {"active":2,"revoked":7},
  "sessions": {"active":1,"revoked":4}
}
```

### `GET /v1/admin/obs/nodes`

```json
200 OK
{
  "items": [
    {"id":"uuid","name":"node-1","active":true,"vpcpus":4,"mem_mb":8192,"max_concurrency":16,"admission_ceiling_mb":47600,"last_heartbeat_at":"2026-08-10T11:59:58Z","created_at":"2025-09-01T00:00:00Z"}
  ],
  "next_cursor": ""
}
```

### `GET /v1/admin/obs/nodes/{name}/heartbeats`

Query: `?since=<rfc3339>&limit=<n>` — defaults to 30m, hard cap 24h,
default 200, cap 2000.

```json
200 OK
{
  "node_id": "uuid",
  "name": "node-1",
  "since": "2026-08-10T11:30:00Z",
  "since_clamped": false,
  "heartbeats": [
    {"received_at":"2026-08-10T11:59:58Z","last_heartbeat_at":"2026-08-10T11:59:57Z","source":"schedd","gap_to_previous_ms":30000,"missed":false,"stale":false}
  ]
}
```

## 7. Risks

- `state.Account` carries sealed-blob MFA fields. Any direct
  `json.Marshal` leaks them. Pinned via projection helper + grep-based
  security tests (`handlers_admin_obs_security_test.go`).
- Pagination cap drift between `pkg/api/paging.go` (25/100) and the new
  surface (200/500). Documented the operator-vs-customer divergence
  in this ADR; cap pinned in `pkg/api/limits.go`.
- `routeExclude` lists drift apart. PR description must call out
  "two-list sync" as a checklist item.
- Migration slot 00174: verify no parallel PR is in flight; otherwise
  drop a fence per `041-migration-slot-reservation.md`.
- `/v1/compute-nodes*` and `/v1/admin/obs/nodes*` co-exist for one
  release; PR #3 marks the older path with `Deprecation`, then 410
  Gone in a follow-up.
- `?include_pii=1` audit-row emit is wired in PR #3, not PR #1. Until
  PR #3 lands, `?include_pii=1` is operational but not auditable.
  This is a documented gap; PR #1 marks it as such on the wire
  response (`audit_emitted: false` in PR #1, `true` in PR #3).

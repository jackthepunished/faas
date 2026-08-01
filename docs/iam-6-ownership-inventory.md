# IAM-6 ownership inventory

Issue #190 (IAM-6) introduces an `orgs` table as the tenant. Every existing
`account_id` column in the schema must be classified so the schema and the
code migration in PR 2 are unambiguous. This document is checked in next
to the ADR and is the source of truth for PR 2 — handlers and tests must
follow this classification.

## A. Identity-owned (stays on `accounts`)

These rows describe the human authenticator. They remain keyed by
`account_id` and do **not** receive an `org_id` column.

| Table | Why it stays account-owned |
|---|---|
| `accounts` | The identity itself. |
| `account_passwords` | One credential row per account. |
| `account_mfa` | TOTP / recovery secrets — bound to the human. |
| `oauth_links` | Provider-side identity binding. |
| `sessions` | Per-device dashboard session row (IAM-3). |
| `login_tokens` | One-shot login tokens (magic links, password reset). |
| `cli_auth_codes` | Device-code pairing. |
| `account_status` / `account_credits` / `credit_ledger_entries` | Operator-side identity columns where the FK root is the account, not a tenant. |

These tables continue to be deleted with the account (the existing
GDPR cascade). Removing the account also removes these rows.

## B. Tenant roots (gain nullable `org_id` in PR 2)

These rows describe shared tenant state and must end up keyed by
`org_id`. PR 2 adds a nullable `org_id` column; PR 3 backfills and
mirrors writes; PR 9 promotes the column to NOT NULL.

| Table | Notes |
|---|---|
| `apps` | Tenant root for deployments, secrets, env, alerts. |
| `projects` | Already keyed by `account_id`; takes a nullable `org_id` and joins back through apps in PR 7. |
| `custom_domains` | Verification is per-tenant. |
| `api_keys` | Key creator's `account_id` stays for attribution, but authn also requires `org_id` (PR 6). |
| `instances` | Written by `schedd`; PR 7 derives `org_id` from the joined app. |
| `usage_minutes` | Aggregation key moves from `account_id` to `org_id`; the actor's `account_id` remains as telemetry. |
| `usage_daily` / `snapshot_storage_daily` | Mirrors `usage_minutes`. |
| `builds` | Per-app; inherits tenancy through `app_id`. |
| `deployments` | Inherits through `app_id`; storing `org_id` denormalised is **not** added unless a measured hot-path requires it. |
| `crons` | Inherits through `app_id`. |
| `alert_rules` | Per-app + account-wide; account-wide rules need `org_id` directly. |
| `invocations` | Inherits through `app_id`. |
| `events` (audit) | The `subject` stays the actor account id; `data` carries `org_id` from PR 4 onward. |
| `gdpr_requests` | One row per account export / delete request; the account id remains the FK root. |
| `invoices` | Move from `accounts.stripe_*` to `orgs.stripe_*`; backfill copies the existing values. |
| `app_secrets` / `app_env` | Inherit through `app_id`. |
| `github_installations` | Install-binding identity; takes `org_id` directly (PR 7). |
| `webhooks` | Per-account GitHub webhooks; become per-org. |
| `snapshots` | Image rows already keyed through the app; tenancy derives via `app.org_id`. |
| `compute_nodes` | Host-side, not customer-facing — stays global. |

## C. Children that inherit tenancy through a root FK

These rows describe work that is already gated by the parent row's
ownership check (e.g. `LoadApp` validates `app.AccountID == acct.ID`).
They do **not** get an explicit `org_id` column. Authorization joins
through the root.

| Table | Inherits through |
|---|---|
| `deployments` | `app_id` |
| `app_secrets` | `app_id` |
| `app_env` | `app_id` |
| `crons` | `app_id` |
| `invocations` | `app_id` |
| `snapshots` | `app_id` |
| `instances` | `app_id` |
| `builds` | `deployment_id` → `app_id` |

If a measured hot-path needs to skip the join, PR 7 may add a
denormalised `org_id` column on these tables, but the default is
*inherit through the root* — fewer columns, fewer invariants, fewer
migrations.

## D. Special: `org_invitations.invited_by` and actor FKs

Audit attribution must survive membership removal. The actor FK
(`invited_by`, audit `data.actor_account_id`, etc.) uses
`ON DELETE SET NULL` so removing the actor's membership does not
lose history. Actor identity is preserved by the AEAD-bound envelope
in `sessions` for live attribution, and by a `data` JSONB snapshot in
the audit row for historical events.

## E. Approval / change-control table

| Decision | Owner | Status |
|---|---|---|
| Add `org_id` to `apps` (nullable → NOT NULL in PR 9) | apid | approved by this ADR |
| Move `stripe_*` columns to `orgs` | apid + billing providers | approved by this ADR; PR 7 carries the change |
| Personal-org automatic backfill | apid | approved; PR 3 |
| Header-based active-org selection | n/a | rejected (see ADR-061 §Rejected alternatives) |
| Permanent account fallback | n/a | rejected (see ADR-061 §Rejected alternatives) |
| Per-seat pricing in this milestone | n/a | rejected; depends on financial model |
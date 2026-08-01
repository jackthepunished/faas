# IAM-6 ownership inventory

Issue #190 (IAM-6) introduces an `orgs` table as the tenant. Every existing
`account_id` column in the schema must be classified so the schema and the
code migration in PR 2 are unambiguous. This document is checked in next
to the ADR and is the source of truth for PR 2 — handlers, schemas, and
tests must follow this classification verbatim.

Cross-checked against every `create table …` statement in
`migrations/*.sql` that mentions `account_id` (slots 00001..00085).
Tables that look topical but have **no** `account_id` column (e.g.
`deployment_logs` keyed on `deployment_id`, host-side
`compute_node_heartbeats`) are intentionally out of scope for the
inventory.

## A. Identity-owned (stays on `accounts`)

These rows describe the human authenticator. They remain keyed by
`account_id` and do **not** receive an `org_id` column. The existing
GDPR cascade continues to delete these with the account.

| Table | Migration | Why it stays account-owned |
|---|---|---|
| `accounts` | `00001_init.sql` | The identity itself. MFA columns (`account_mfa_secret`, `mfa_recovery_codes_hash`) live on `accounts` via `00049_account_mfa.sql`. |
| `account_passwords` | `00039_account_passwords.sql` | One credential row per account. |
| `oauth_links` | `00038_oauth_links.sql` | Provider-side identity binding. |
| `sessions` | `00057_sessions.sql` | Per-device dashboard session row (ADR-039 / IAM-3). |
| `login_tokens` | `00005_login_tokens.sql` | One-shot login tokens (magic links, password reset). |
| `cli_auth_codes` | `00014_cli_auth_codes.sql` | Device-code pairing. |
| `account_credits` | `00054_account_credits.sql` | Operator-side identity column where the FK root is the account, not a tenant. |
| `credit_ledger` | `00054_account_credits.sql` | Immutable rows tied to a credit issuance; the FK root is `account_credits` → account. |

Dunning fields (`past_due_at`, etc.) and MFA columns live as columns
on `accounts` itself (added by `00013_account_dunning_and_quota_warning.sql`
and `00049_account_mfa.sql`), so they are inherited by section A through
the `accounts` row and do **not** need a separate line.

## B. Tenant roots (gain nullable `org_id` in PR 2)

These rows describe shared tenant state and must end up keyed by
`org_id` by the time PR 9 sets the columns `NOT NULL`. PR 2 adds a
nullable `org_id` column; PR 3 backfills and mirrors writes; PR 9
promotes the column to `NOT NULL`.

| Table | Migration | Inherits tenancy through | Notes |
|---|---|---|---|
| `apps` | `00001_init.sql` | itself (root) | Tenant root for deployments, secrets, env, alerts, crons. |
| `projects` | `00074_projects_and_workloads.sql` | itself (root) | Phase-4 decomposition: tenant root keyed by `org_id` directly. |
| `custom_domains` | `00001_init.sql` | via `app_id` | Verification is per-tenant. |
| `api_keys` | `00001_init.sql` | creator's account for attribution | Authn also requires `org_id` (PR 6); creator's `account_id` stays for audit. |
| `instances` | `00001_init.sql` | via `app_id` | Written by `schedd`; PR 7 derives `org_id` from the joined app. |
| `usage_minutes` | `00001_init.sql` | via `app_id` | Aggregation key moves from `account_id` to `org_id`; the actor's `account_id` remains as telemetry. |
| `usage_daily` | `00066_usage_minutes_egress.sql` | (mirror) | Mirrors `usage_minutes`. |
| `invoices` | `00048_invoices.sql` | itself (root) | Move from `accounts.stripe_*` to `orgs.stripe_*`; backfill copies existing values. |
| `stripe_push_dedupe` | `00004_stripe_push_dedupe.sql` | itself (root) | PK `(account_id, hour)`; follows the org under PR 7 so the dedupe window aligns with the billing customer. |
| `paddle_overage_dedupe` | `00034_paddle_overage_dedupe.sql` | itself (root) | PK `(account_id, month)`; same rationale as `stripe_push_dedupe`. |
| `app_secrets` | `00008_app_secrets.sql` | via `app_id` | Inherits through the app root. |
| `app_envs` | `00061_app_envs.sql` | via `app_id` | Inherits through the app root. |
| `alert_rules` | `00062_alert_rules.sql` | either | App-bound rules inherit via `app_id`; account-wide rules (`app_id IS NULL`) gain `org_id` directly. |
| `recent_build_claims` | `00044_recent_build_claims.sql` | itself (root) | Drives B2.2 builder fairness; under org tenancy the fairness window is one per org, not one per account. |
| `builder_usage` | `00068_builder_usage.sql` | via `app_id` | Telemetry keyed on `account_id` + `app_id`; PR 7 derives `org_id` from the joined app. |
| `crons` | `00001_init.sql` | via `app_id` | Inherits through the app root. |
| `invocations` | `00030_invocations.sql` | via `app_id` | Inherits through the app root. |
| `github_installations` | `00059_github_installations.sql` | itself (root) | Per-install identity; takes `org_id` directly when an installation is shared. |
| `gdpr_requests` | `00019_gdpr_requests.sql` | itself (root) | One row per account export / delete request; the account id remains the FK root, but the request scope may include org-scoped resources. |

The tables listed under "via `app_id`" derive tenancy through the
joined app — see section C. PR 7 may add a denormalised `org_id`
column on a per-table basis if a measured hot-path warrants it; the
default is *inherit through the root*.

## C. Children that inherit tenancy through a root FK

These rows describe work that is already gated by the parent row's
ownership check (e.g. `LoadApp` validates `app.AccountID == acct.ID`).
They do **not** get an explicit `org_id` column by default.
Authorization joins through the root.

| Table | Inherits through | Notes |
|---|---|---|
| `deployments` | `app_id` | Already covered by `apps.LoadApp`. |
| `builds` | `deployment_id` → `app_id` | Already covered by deployments. |
| `snapshots` | `deployment_id` → `app_id` | Image rows; tenancy derives via `app.org_id`. |
| `deployment_logs` | `deployment_id` → `app_id` | Stream rows; no `account_id` is stored, so no denormalisation needed. |

If a measured hot-path needs to skip the join, PR 7 may add a
denormalised `org_id` column on these tables, but the default is
*inherit through the root* — fewer columns, fewer invariants, fewer
migrations.

The "via `app_id`" rows in section B (custom_domains, instances,
usage_minutes, app_secrets, app_envs, crons, invocations,
builder_usage) are the same tables whose tenancy follows through
the app root. They appear in section B because they gain a nullable
`org_id` column in PR 2 for query convenience, but they do not gain
ownership semantics independently of `app_id`. PR 2 may choose to
omit the `org_id` column on any of them by treating them as
section C; the default is section B to keep the wake-hot-path
single-join.

## D. Special: `org_invitations.invited_by` and actor FKs

Audit attribution must survive membership removal. The actor FK
(`org_invitations.invited_by` introduced in PR 2, audit
`data.actor_account_id` introduced in PR 2) uses `ON DELETE SET NULL`
so removing the actor's membership does not lose history. Actor
identity is preserved by the AEAD-bound envelope in `sessions` for
live attribution, and by a `data` JSONB snapshot in the audit row for
historical events.

The existing `events` table (`00001_init.sql:129`) has `actor text`
(no FK) and a `data jsonb` snapshot field. PR 2 introduces an
optional `actor_account_id uuid` reference column on `events` (NULL
when the actor is a service principal or system identity); pre-PR-2
rows keep `actor text` unchanged. The ON DELETE SET NULL contract
applies to the new column only — existing rows are immutable.

## E. Approval / change-control log

Decisions captured during PR 1 (ADR-061 + this inventory) that
future PRs must honour. PRs that change a row's classification must
update this table and add an ADR increment in the same PR.

| Decision | Owner | Status |
|---|---|---|
| Add `org_id` to `apps` (nullable → NOT NULL in PR 9) | apid | approved by ADR-061 |
| Move `stripe_*` columns to `orgs`; tenant-side dedupe tables (`stripe_push_dedupe`, `paddle_overage_dedupe`) follow the org under PR 7 | apid + billing providers | approved; PR 7 |
| Personal-org automatic backfill | apid | approved; PR 3 |
| Header-based active-org selection | n/a | rejected (ADR-061 §Rejected alternatives) |
| Permanent legacy account fallback | n/a | rejected (ADR-061 §Rejected alternatives) |
| Per-seat pricing in this milestone | n/a | rejected (ADR-061 §Rejected alternatives) |
| Folding IAM-1 / IAM-2 / IAM-3 work into IAM-6 | n/a | rejected (ADR-061 §Rejected alternatives) |
| Slug regex is shared via a `pkg/api` constant (`OrgSlugPattern`) | apid | approved; PR 5 |
| `OrgMembersMax` semantics: active members only (`removed_at IS NULL`) | apid + schedd | approved; PR 2 ships the `removed_at` column, PR 5 sets the partial-index predicate |
| `recent_build_claims` fairness window = one per org, not one per account | builderd | approved; PR 7 |
| Legacy API keys cannot auto-promote to org-bound | apid | approved; customer must mint a successor via `/v1/orgs/{slug}/keys` (PR 6) |
| No role may demote or remove the only owner (ownership transfer is the sole path) | apid | approved; PR 2 + PR 5 RBAC checks |

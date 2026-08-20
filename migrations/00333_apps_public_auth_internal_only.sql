-- filename: 00333_apps_public_auth_internal_only.sql
-- +goose Up
-- +goose StatementBegin

-- ADR-119: per-app ingress 'internal_only' mode. Closes the
-- fourth bullet of the canonical ingress-control matrix
-- (Public / Organization members only / Selected IP ranges /
-- Internal Gregale services only). ADR-118 reserved the enum
-- value as a future extension point and called out the
-- prerequisites in §Out-of-scope (line 239-243); ADR-119 is
-- that follow-on.
--
-- When apps.public_auth_mode='internal_only', every public
-- request must carry an Authorization: Bearer JWT with
-- aud='gregale.internal' signed by a Gregale daemon's Ed25519
-- key (gatewayd-internal holds the per-service public-key
-- allowlist). Anything else 403s at
-- pkg/gateway/handler.go::applyIngressInternalSvc (runs after
-- applyIngressIPAllowlist, before applyEdgeRuleIP).
--
-- Schema change is a CHECK widening only — no new column.
-- The token lives on the request, not on the app row; the
-- app row just pins the policy. Mirroring the pattern from
-- migrations/00326_apps_public_auth_ip_allowlist.sql:63-65
-- (the ip_allowlist widening), we DROP + ADD the constraint
-- with the full vocabulary literal because Postgres 15 (CI)
-- rejects ADD CONSTRAINT IF NOT EXISTS.
--
-- Slot note: 00333. Bridge fences at 00327-00332 carry the
-- slot past open PRs that own the intervening slots:
--   - PR #990 (open, ADR-117 PR-C): real at 00327_app_secret_value_hash.sql
--   - PR #991 (open, ADR-117 mega-C): real at 00328_preview_destroy_commented_at.sql
--   - PR #1000 (open, ADR-120 consumer_keys): real at 00329_consumer_keys.sql
--   - PR #1005 (open, api-contract-diff PR-A): real at 00330_deployment_openapi_snapshots.sql
-- PRs #990/#991/#1000/#1005 own those slots; my fences are
-- the ADR-041 reservation-fence pattern. If any of those
-- PRs merges first, the same-named fence gets dropped on
-- rebase per ADR-041 (whichever PR merges first keeps the
-- fence; the other drops). The synthetic-merge embed
-- always has the file from main, so the embed is contiguous.

alter table apps drop constraint if exists apps_public_auth_mode_chk;
alter table apps add constraint apps_public_auth_mode_chk
  check (public_auth_mode in ('open','bearer','basic','ip_allowlist','internal_only'));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Down-migrate ordering is load-bearing. We must DROP any
-- rows whose public_auth_mode='internal_only' BEFORE
-- narrowing the CHECK constraint: any such row would fail
-- the new CHECK with SQLSTATE 23514 and abort the entire
-- Down with the constraint swap half-applied. Pattern from
-- migrations/00326_apps_public_auth_ip_allowlist.sql:131-141
-- (CHECK widening follows the same ordering: data →
-- constraint, never constraint → data).
--
-- The actual mode UPDATE that drops the rows is the
-- responsibility of an operator pre-Down (the migration
-- itself cannot safely UPDATE because the row may have
-- been the only one in the customer's account, and silently
-- deleting would lose data). The Down section narrows the
-- CHECK and accepts a SQLSTATE 23514 if rows remain — the
-- operator must clear them before the Down can complete.
-- Documented here for the operator; pin via
-- DownGrade_NarrowsAndAcceptsRowsPresent in the companion
-- test.

alter table apps drop constraint if exists apps_public_auth_mode_chk;
alter table apps add constraint apps_public_auth_mode_chk
  check (public_auth_mode in ('open','bearer','basic','ip_allowlist'));

-- +goose StatementEnd

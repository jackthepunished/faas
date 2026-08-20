-- filename: 00313_apps_public_auth_ip_allowlist.sql
-- +goose Up
-- +goose StatementBegin

-- ADR-118: per-app ingress IP allowlist. The new public_auth_mode
-- enum value 'ip_allowlist' (ADR-079's reserved extension slot)
-- pins a per-app ingress CIDR allowlist. When set, every public
-- request to the app's hostname must originate from a client IP
-- inside apps.public_auth_ip_allowlist — anything else 403s at
-- pkg/gateway/handler.go::applyIngressIPAllowlist (runs before
-- applyEdgeRuleIP, before wake).
--
-- Schema choice: cidr[] column on apps, NOT a child table. The
-- read pattern is "all CIDRs for one app on every request" —
-- pk lookup only, no join needed. The write pattern is "atomic
-- full-replace" — one row UPDATE; a child table needs DELETE +
-- N INSERTs inside a tx. Mirrors the egress allowlist column at
-- migrations/00029_app_egress_allowlist.sql:22 verbatim.
--
-- The per-plan cap is a count of *entries*, validated in the
-- API layer (pkg/api/limits.go PublicAuthIPAllowlistMaxEntries)
-- before SQL. Free/Hobby: 0 (gate closed). Pro: 16. Scale: 64.
--
-- Slot note: 00313. Renumbered twice — 00308 → 00309 → 00313 —
-- because open PRs claimed slots ahead:
--   - PR #988 (merged): real 00304_cors_presets.sql on main.
--     PR #999 must not fence slot 00304 (main owns it).
--   - PR #997 (open): fences 00305-00306, real at 00307.
--     PR #999 renumbered past 00307 to 00309.
--   - PR #990 (open, ADR-117 env-diff PR-C): fences
--     00305-00308, real at 00309_app_secret_value_hash.sql.
--     Cross-PR slot gate flagged 00309 collision; renumbered
--     past 00309 to 00313, adding reservation fences 00310-00312
--     to bridge the gap. Per docs/adr/README.md "migrations
--     are append-only and contiguous" + the precedent set by
--     PR #984 (issue #977, 8-hop renumber) for collision rebumping.

alter table apps
  add column if not exists public_auth_ip_allowlist cidr[] not null default '{}';

-- Widen the existing CHECK constraint to include the new enum
-- value. Pattern from migrations/00254_edge_rules_kind_budget.sql:67-71.
-- Postgres 15 (CI) rejects `ADD CONSTRAINT IF NOT EXISTS`, so the
-- canonical shape is DROP + ADD with the full vocabulary literal.
alter table apps drop constraint if exists apps_public_auth_mode_chk;
alter table apps add constraint apps_public_auth_mode_chk
  check (public_auth_mode in ('open','bearer','basic','ip_allowlist'));

-- Per-entry guard: family must be v4 or v6, mask must be non-zero.
-- Postgres CHECK constraints cannot reference aggregate functions
-- (bool_and/family(cidr) is not allowed) — that approach compiles
-- into a SQL parse error: `column "cidr" does not exist`. So the
-- enforcement is a BEFORE-row trigger. Mirrors the egress trigger
-- at migrations/00033_app_egress_allowlist_v6.sql:19-64 verbatim.
--
-- Empty array short-circuits at the top — the canonical "no rule"
-- state is never validated. errcode 23514 (check_violation) +
-- constraint name `apps_public_auth_ip_allowlist_cidr` so callers
-- can match on errors.As(err, &pgErr) → pgErr.ConstraintName.

drop trigger if exists apps_public_auth_ip_allowlist_cidr on apps;
drop function if exists apps_public_auth_ip_allowlist_cidr_check();

create or replace function apps_public_auth_ip_allowlist_cidr_check()
  returns trigger
  language plpgsql
  as $$
declare
  bad cidr;
begin
  if new.public_auth_ip_allowlist is null or cardinality(new.public_auth_ip_allowlist) = 0 then
    return new;
  end if;
  -- Per-entry guards: family must be v4 or v6, mask must be non-zero.
  -- The /0 reject closes the same hole as the egress allowlist's
  -- chain-policy accept: an operator cannot pin "the entire
  -- address space" — that is the default-pass posture's job, not
  -- the allowlist's. Two narrow selects (one per guard) keep the
  -- error messages specific; a combined select with bool_or would
  -- conflate family and masklen failures.
  for bad in
    select c
      from unnest(new.public_auth_ip_allowlist) c
     where family(c) not in (4, 6)
     limit 1
  loop
    raise exception 'apps_public_auth_ip_allowlist: only v4 or v6 CIDRs (got family % for %)', family(bad), bad
      using errcode = '23514',
            constraint = 'apps_public_auth_ip_allowlist_cidr';
  end loop;
  for bad in
    select c
      from unnest(new.public_auth_ip_allowlist) c
     where masklen(c) = 0
     limit 1
  loop
    raise exception 'apps_public_auth_ip_allowlist: rejected % (masklen /0; ADR-118 non-/0 contract)', bad
      using errcode = '23514',
            constraint = 'apps_public_auth_ip_allowlist_cidr';
  end loop;
  return new;
end;
$$;

create trigger apps_public_auth_ip_allowlist_cidr
  before insert or update of public_auth_ip_allowlist on apps
  for each row
  execute function apps_public_auth_ip_allowlist_cidr_check();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Down-migrate ordering is load-bearing. We must DROP the column
-- BEFORE narrowing the CHECK constraint: any row with
-- public_auth_mode='ip_allowlist' (set by a post-Up customer PATCH)
-- would otherwise fail the new CHECK with SQLSTATE 23514 and abort
-- the entire Down with the trigger + function already dropped but
-- the column and the constraint swap half-applied. Pattern from
-- migrations/00254_edge_rules_kind_budget.sql:124-141 (CHECK
-- widening follows the same ordering: data → constraint, never
-- constraint → data).
--
-- Step order:
--   1. drop trigger — no longer needed once the column is gone
--   2. drop function — paired with the trigger
--   3. drop column  — clears mode='ip_allowlist' rows AND their
--                     CIDR data in one shot, so step 4 has no rows
--                     to fail on
--   4. drop + re-add CHECK narrowed to the pre-ADR-118 enum
--
-- Reverse of the Up section, mirroring migrations/00033's Down.

drop trigger if exists apps_public_auth_ip_allowlist_cidr on apps;
drop function if exists apps_public_auth_ip_allowlist_cidr_check();

alter table apps drop column if exists public_auth_ip_allowlist;

alter table apps drop constraint if exists apps_public_auth_mode_chk;
alter table apps add constraint apps_public_auth_mode_chk
  check (public_auth_mode in ('open','bearer','basic'));

-- +goose StatementEnd

-- +goose Up
-- +goose StatementBegin
--
-- 00054_account_credits.sql — issue #279 (BILLING: refunds / credits /
-- overage-cap toggle).
--
-- Slot renumbered 00049 → 00054 after PR #341 (state coverage 70) landed
-- `00049_account_mfa.sql` on main. Walking past the collision keeps both
-- migrations safe; the previous renumbers (00050 → 00051 → 00053 → 00054)
-- are documented in memory `migration-slot-collisions-across-prs.md`.
--
-- Adds three concerns in one transaction so a partial failure can't
-- leave the billing surface half-built:
--
--   1. account_credits — a positive-cents balance issued by an operator
--      via POST /v1/admin/accounts/{id}/credits. cents_remaining is
--      decremented at consumption time (consumption reducer is the
--      PR #323 invoice-finalization follow-up; this PR only lands the
--      issuance surface). serves the "goodwill credit" / "outage
--      apology" use case without leaving the platform.
--
--   2. credit_ledger — append-only audit log of every credit delta.
--      Mirrors the events-table pattern: no UPDATE/DELETE allowed by
--      code convention (no surface grants a write), and ON DELETE
--      CASCADE on account_id so GDPR DeleteAccount scrubs both tables
--      in the same transaction. Reason text is operator-supplied and
--      rides through the audit row directly.
--
--   3. accounts.overage_cap_cents — nullable per-account ceiling that
--      meterd compares against the current-month derived overage
--      (sum of usage_minutes.mb_seconds since the UTC month start) and
--      skips the overage-row insert when the cap is met. In-budget
--      usage still accrues; the cap is layered on top of the existing
--      Free-stop / paid-warning ladder. NULL = no cap (default),
--      0 = "no overage allowed", > 0 = explicit monthly ceiling.
--      Mirrors the apps.min_instances pattern (00011) — a per-account
--      override that does not live in pkg/api/limits.go.
--
-- Money: integer cents (provider native). €0.01/GB-h → 1 GB-h = 100
-- cents. Never float on money; the cap check is integer arithmetic.
--
-- Idempotent (IF NOT EXISTS / drop-then-add for the constraint) so the
-- migration is safe to re-run during local development, matching the
-- style of 00011 and 00046.

create table if not exists account_credits (
  id              uuid primary key default gen_random_uuid(),
  account_id      uuid not null references accounts(id) on delete cascade,
  cents_remaining bigint not null check (cents_remaining >= 0),
  reason          text not null check (char_length(reason) between 3 and 500),
  created_at      timestamptz not null default now(),
  expires_at      timestamptz
);
create index if not exists account_credits_account_active_idx
  on account_credits (account_id, expires_at, cents_remaining)
  where cents_remaining > 0;

create table if not exists credit_ledger (
  id              uuid primary key default gen_random_uuid(),
  account_id      uuid not null references accounts(id) on delete cascade,
  credit_id       uuid not null references account_credits(id) on delete cascade,
  delta_cents     bigint not null check (delta_cents <> 0),
  reason          text not null,
  actor           text not null,
  created_at      timestamptz not null default now()
);
create index if not exists credit_ledger_account_created_idx
  on credit_ledger (account_id, created_at desc);

-- Per-account overage ceiling (nullable). drop-then-add for the
-- CHECK because PG does not accept ADD CONSTRAINT IF NOT EXISTS.
alter table accounts drop column if exists overage_cap_cents;
alter table accounts add column overage_cap_cents bigint;
alter table accounts drop constraint if exists accounts_overage_cap_cents_chk;
alter table accounts add constraint accounts_overage_cap_cents_chk
  check (overage_cap_cents is null or overage_cap_cents >= 0);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
alter table accounts drop constraint if exists accounts_overage_cap_cents_chk;
alter table accounts drop column if exists overage_cap_cents;
drop table if exists credit_ledger;
drop table if exists account_credits;
-- +goose StatementEnd

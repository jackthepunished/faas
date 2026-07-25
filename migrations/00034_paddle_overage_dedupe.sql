-- +goose Up
-- +goose StatementBegin

-- M7 follow-up: Paddle overage dedupe table (migration 00034; planned
-- as 00033 but renumbered at PR-creation time because
-- 00033_app_egress_allowlist_v6.sql landed on main first). meterd's
-- daily pusher
-- writes one row per (account, month) AFTER a successful Paddle
-- CreateTransaction POST, and reads the same row BEFORE the POST
-- as a no-op gate. The within-process `acc.flushed` flag still
-- covers the same-process redelivery case; this table closes the
-- crash-between-POST-and-stamp window that lets the next month's
-- first push re-POST a month that was already billed on the
-- merchant dashboard.
--
-- Paddle has no metered-subscription equivalent to Stripe (so
-- overage is a flat-rate line item posted at month-rollover),
-- hence the PK grain is (account_id, month) instead of
-- (account_id, hour). `month` is the calendar-month start,
-- normalized to UTC at the call site (calendarMonthStart in
-- pkg/billing/paddle/usage.go).
--
-- Schema shape mirrors migrations/00004_stripe_push_dedupe.sql:
-- FK ON DELETE CASCADE so a GDPR right-to-erasure DeleteAccount
-- drops the dedupe row alongside its parent account.
--
-- Index on (month) is for retention sweeps. The Paddle push is
-- monthly, so the table's growth rate is orders of magnitude
-- slower than stripe_push_dedupe; the index is there for
-- symmetry with the Stripe table, not for an immediate need.

create table if not exists paddle_overage_dedupe (
  account_id  uuid        not null references accounts(id) on delete cascade,
  month       timestamptz not null,
  pushed_at   timestamptz not null default now(),
  primary key (account_id, month)
);

create index if not exists paddle_overage_dedupe_month_idx
  on paddle_overage_dedupe (month);

-- +goose StatementEnd
-- +goose Down
-- +goose StatementBegin
drop table if exists paddle_overage_dedupe;
-- +goose StatementEnd

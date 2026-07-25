-- +goose Up
-- +goose StatementBegin

-- Per-window claim state machine for Paddle overage push. Replaces
-- the month-scoped dedupe shape from migration 00034 with a
-- window-scoped pending/completed claim that closes the
-- underbilling defect and the cross-process race that PR #204
-- shipped with.
--
-- Why per-window: PR #204's meterd loop reads
-- state.UsageByHour(start, end) (window-scoped) and hands the
-- per-window sum to provider.PushUsageRecord, but the dedupe row
-- at the time was keyed on calendarMonthStart(hour)
-- (month-scoped). The two scopes disagreed: the first positive
-- window in a month POSTed and recorded the row; every subsequent
-- window in the same month saw `already == true` and returned
-- nil. Customers were underbilled whenever their usage crossed a
-- window boundary. The fix keys the dedupe row by (account_id,
-- window_start) where window_start is hour.UTC().Truncate(Hour),
-- mirroring Stripe's stripe_push_dedupe grain (migrations/00004).
--
-- Why pending/completed: PR #204's Has→POST→Record was a
-- check-then-act sequence. Two meterd pods racing on the same
-- window both saw no row, both POSTed, both then raced to record
-- — a TOCTOU double-bill window. Until Paddle's server-side
-- Idempotency-Key collapse ships (ADR-032 §4), the durable guard
-- is an atomic SQL claim: the first process to
-- UPDATE … SET state='pending' WHERE state IS NULL wins; the
-- loser sees 0 rows and skips. After the POST succeeds, the same
-- process UPDATE … SET state='completed' WHERE state='pending'
-- stamps the terminal row. A stale-pending reaper at meterd boot
-- resets rows whose claimed_at is older than the lease window so
-- a crashed pod doesn't strand claims forever.
--
-- Mirrors the ClaimInvocation pattern at pkg/state/pgstore.go:1297.
--
-- Slot discipline: per migration-slot-renumber-at-pr-creation,
-- 00041 was the next free slot on origin/main after the merge of
-- PR #180 (which occupied 00038_oauth_links + 00039_account_passwords).
-- The original PR #204 plan called this 00036; the fix-PR renumbered
-- once to 00037, then again to 00039 after PR #180's 00035/36/37
-- landed on main, then a third time to 00041 after PR #180's
-- 00038/00039 landed on main. Each renumber is captured in the
-- squash-commit history of feat/paddle-full-enable.

alter table paddle_overage_dedupe
  add column if not exists window_start timestamptz,
  add column if not exists state text not null default 'completed'
    check (state in ('pending','completed')),
  add column if not exists claimed_at timestamptz,
  add column if not exists claimed_by text;

-- Backfill: pre-existing rows from PR #179 / PR #204 are terminal
-- by construction (the only writer path inserts only after a
-- successful POST). The state column defaults to 'completed' for
-- legacy rows so the new claim path doesn't trip over NULL.
-- We do NOT backfill window_start — legacy rows are keyed on
-- (account_id, month) and the new claim path keys on
-- (account_id, window_start). The two scopes don't overlap, and
-- legacy rows are intentionally deleted below before the PK
-- switch.
update paddle_overage_dedupe
  set state = 'completed'
  where state is null;

-- Drop the old (account_id, month) PK. It conflicts with the new
-- (account_id, window_start) PK because legacy rows have
-- window_start = NULL — dropping is the only way to remove the
-- constraint, and the table is small (per-month flushes per paid
-- account; cf. migrations/00034).
alter table paddle_overage_dedupe
  drop constraint if exists paddle_overage_dedupe_pkey;

-- Delete legacy rows. They were keyed by (account_id, month) and
-- would otherwise collide with the new PK semantics. The table
-- was added in PR #179 and the only writer inserts on a
-- successful POST; on a fresh install there are no legacy rows
-- to begin with. On an upgraded install that ran PR #179 + PR
-- #204 against the live merchant, deleting these rows allows the
-- next meterd tick to re-POST — but the Idempotency-Key
-- (faas-overage-<acctID>-<YYYY-MM>) is identical to what was
-- originally sent, so Paddle collapses the redelivered POST as
-- idempotent once server-side support ships. Until then, this is
-- a one-time re-bill of any month that already flushed — the
-- merchant dashboard will show duplicate Transactions. Operators
-- with PR #204 in production should run a manual reconciliation
-- before applying this migration; operators without PR #204 (only
-- PR #179) had no production Paddle flushes because PR #204 was
-- the first to wire up a real Paddle provider, so this delete is
-- a no-op for them.
delete from paddle_overage_dedupe where window_start is null;

-- New PK. The new claim path keys on (account_id, window_start).
alter table paddle_overage_dedupe
  add primary key (account_id, window_start);

-- Partial index for the boot-time stale-pending reaper. The
-- reaper query is `WHERE state='pending' AND claimed_at < now() -
-- interval '<lease>'` — a partial index over the small set of
-- currently-pending rows is the right shape. A full index on
-- (state, claimed_at) would be wasted space because completed
-- rows dominate the table.
create index if not exists paddle_overage_dedupe_pending_idx
  on paddle_overage_dedupe (claimed_at)
  where state = 'pending';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- The Down path is intentionally destructive (drops the table) to
-- match migrations/00034's down (drop table if exists). Operators
-- who downgrade past this migration lose the dedupe row
-- history; the next meterd tick re-POSTs any month that already
-- flushed, identical to the legacy-row-deletion concern in the
-- Up path.

drop index if exists paddle_overage_dedupe_pending_idx;
alter table paddle_overage_dedupe drop constraint if exists paddle_overage_dedupe_pkey;
drop table if exists paddle_overage_dedupe;

-- +goose StatementEnd

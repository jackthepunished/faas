-- +goose Up
-- +goose StatementBegin

-- Stamp the integer mb_seconds the meterd loop pushed on the
-- terminal row of paddle_overage_dedupe. Currently this value lives
-- only on the Paddle merchant dashboard via CustomData, which makes
-- per-month reconciliation join-heavy (usage_minutes ⨝
-- paddle_overage_dedupe doubled against merchant CSV exports).
-- Materializing it here closes the gap.
--
-- Nullable: pre-existing 00041 rows have no value. The downstream
-- tests + CLI treat NULL as "we know a flush happened, but the
-- quantity is on the merchant side" — surfacing NULL beats
-- backfilling an arbitrary zero.
--
-- The complete path (paddle.go:85 CompletePaddleOverageWindow) and
-- the meterd-side SDK call (pushUsageRecord → flushOverageLocked)
-- are the only writers; both code paths now stamp this column.
-- Column order matters for the column-list-shadow trap at
-- pkg/state/pgstore.go (see pgstoreCreateApp pattern at the
-- column-list comment in PR #521 / memory): the meterd flow does
-- not list columns explicitly, it uses the default-stamped
-- shape, so a single nullable column add is safe.
--
-- Issue #686 closes when this lands + the production stamping
-- change in pkg/state/pgstore.go:9920 lands in the same PR.

alter table paddle_overage_dedupe
  add column if not exists pushed_mb_seconds bigint;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

alter table paddle_overage_dedupe drop column if exists pushed_mb_seconds;

-- +goose StatementEnd

-- filename: 00511_mirror_invocation_results_completed_at_idx.sql
-- +goose Up
-- +goose StatementBegin

-- issue #72 / ADR-133 / ADR-125 PR-A3 code-review fix #7
--
-- PR-A3 commit 4 added SweepOldLedgerRows (pkg/mirror/rollup.go)
-- which runs:
--
--     DELETE FROM mirror_invocation_results WHERE completed_at < $1
--
-- on a 7-day cutoff, once per RollupLoop tick (default 5m).
-- The existing index
-- (mirror_invocation_results_rule_time_idx on
-- (mirror_rule_id, completed_at DESC)) does NOT lead with
-- completed_at, so Postgres seq-scans the ledger — a seq-scan
-- over 7 days of per-request inserts (≈8M rows / day at 100 RPS
-- × 1 mirror rule) is a multi-second lock-wait and a 30%+
-- bloat on pg_stat_user_tables. Add a leading-column index
-- on completed_at so the sweep predicate is index-only.
--
-- Partial index on (completed_at) WHERE mirror_rule_id IS NOT
-- NULL — the ledger table's PRIMARY KEY is implied to have
-- every row with a mirror_rule_id (the NOT NULL constraint is
-- added by migration 00386). The partial form is purely a
-- size optimisation; a customer that disables all rules still
-- has zero rows here.

CREATE INDEX IF NOT EXISTS mirror_invocation_results_completed_at_idx
    ON mirror_invocation_results (completed_at)
    WHERE mirror_rule_id IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS mirror_invocation_results_completed_at_idx;
-- +goose StatementEnd

-- filename: 00210_crons_unique_app_schedule_path.sql
-- +goose Up
-- +goose StatementBegin
-- Issue #791 closure / PR-E: enforce that a cron (app_id, schedule, path)
-- triple is unique. The manifest dedupe primitive
-- (pkg/gregalemanifest/manifest.go:165-168) has been asserting this in
-- comments since ADR-090 landed; this migration makes the DB layer the
-- source of truth so concurrent fan-out cannot create two identical
-- crons under any retry topology.
--
-- Existing indexes (crons_app_idx partial WHERE enabled, crons_app_full_idx
-- non-partial from 00051) are unaffected; the new constraint uses the
-- unique btree on (app_id, schedule, path) directly. NULLs in schedule
-- are rejected by the column NOT NULL; path defaults to '/' so the
-- column is never NULL in practice.
--
-- Slot 00210 is the next free slot at branch push time (2026-08-11):
-- main's last real migration is 00206 (PR #819, merged); PR #826
-- (operator-obs-pr4) claims 00207 (compute_node_heartbeats_stats);
-- PR #836 (ADR-091 deployments scope) and PR #829
-- (paddle-live-integration-tests) both renumbered to claim 00209
-- after I previously renumbered to 00208 then 00209. Step past all
-- three to 00210 to clear the cross-PR slot fence.
-- The DO $$ block makes the ADD CONSTRAINT idempotent: replay-safety
-- (migrations/replay_safety_test.go) re-runs every NEW migration against
-- a partially-applied DB where the schema is ahead of goose_db_version
-- (the documented deploy failure mode — see ci.yml line 894-900). Without
-- the IF NOT EXISTS guard, the replay trips SQLSTATE 42P07 ("relation
-- already exists") and the test fails — even though the FIRST apply on a
-- fresh DB succeeds cleanly. Postgres has no `ADD CONSTRAINT IF NOT EXISTS`
-- syntax; the DO block with an EXISTS check is the canonical workaround.
-- This is NOT masking a real bug: the constraint is unconditionally added
-- on a fresh deploy, and the replay path is a defensive belt for the
-- case where the deploy's goose_apply phase half-failed and the operator
-- is replaying.
DO $$
BEGIN
	IF NOT EXISTS (
		SELECT 1 FROM pg_constraint
		WHERE conname = 'crons_app_schedule_path_unique'
		  AND conrelid = 'crons'::regclass
	) THEN
		ALTER TABLE crons ADD CONSTRAINT crons_app_schedule_path_unique UNIQUE (app_id, schedule, path);
	END IF;
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE crons DROP CONSTRAINT IF EXISTS crons_app_schedule_path_unique;
-- +goose StatementEnd
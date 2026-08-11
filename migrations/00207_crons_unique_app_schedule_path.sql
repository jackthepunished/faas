-- filename: 00207_crons_unique_app_schedule_path.sql
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
-- Slot 00207 is free (PR-A landed at 00203, PR #819 fences held 00204 +
-- 00205 and shipped its allowlist at 00206).
ALTER TABLE crons ADD CONSTRAINT crons_app_schedule_path_unique UNIQUE (app_id, schedule, path);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE crons DROP CONSTRAINT crons_app_schedule_path_unique;
-- +goose StatementEnd
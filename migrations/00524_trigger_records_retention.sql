-- +goose Up
-- +goose StatementBegin
--
-- ADR-134 PR-E: trigger_records retention index.
--
-- The trigger_records reaper (pkg/sched/retention_triggers.go)
-- DELETEs rows in (succeeded|dead_letter) whose
-- result_retention_until is in the past. The partial index keeps
-- the scan bounded by the rare rows that actually carry an
-- explicit retention horizon — the reaper skips rows without
-- the override (the trigger-level default retention applies
-- instead, handled by the reaper at the application layer).
--
-- Same shape as migrations/00059_invocations_retention_idx.sql
-- (the parallel index for the invocations reaper); partial
-- because most rows carry NULL retention.
--
CREATE INDEX trigger_records_app_retention_idx
  ON trigger_records (app_id, result_retention_until)
  WHERE result_retention_until IS NOT NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS trigger_records_app_retention_idx;
-- +goose StatementEnd
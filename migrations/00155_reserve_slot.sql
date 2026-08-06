-- filename: 00155_reserve_slot.sql
-- Slot 155 — claimed by PR #697 (issue #554 followup:
-- deployments.parked_reason persistence + audit surface + AC #1
-- metal test, ADR-079 follow-up). PR #697 owns the slot; this
-- branch (issue #695 / ADR-080) renumbered from 00155 to 00156 to
-- avoid a goose "duplicate version 155" collision when both PRs
-- land. The fence body is a no-op `SELECT 1;` per ADR-041 so goose
-- applies it cleanly and writes a row in goose_db_version.
--
-- Drop this fence when PR #697 merges — `git rm
-- migrations/00155_reserve_slot.sql` + drop the test file. Do NOT
-- back-fill this slot with a real migration.

-- +goose Up
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
-- +goose StatementBegin
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
-- +goose StatementBegin
-- +goose StatementEnd
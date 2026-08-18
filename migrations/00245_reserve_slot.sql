-- filename: 00245_reserve_slot.sql
-- +goose Up
-- +goose StatementBegin
-- Co-fence for PR #895 (ADR-099 jobs cluster). PR #895 owns
-- 00245_jobs.sql on its open branch; this fence keeps the
-- embedded slot set contiguous in TestMigrationsContiguous and
-- will be dropped in a follow-up commit when PR #895 merges
-- (per ADR-041 co-fence pattern). Body is a no-op SELECT 1.
SELECT 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
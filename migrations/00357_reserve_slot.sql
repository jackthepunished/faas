-- 00357_reserve_slot.sql — temporary concurrent-PR migration fence.
-- PR #990 (ADR-117 PR-C env-diff + app_secret_value_hash) owns
-- real migration at 00357. PR #1012 (stages-prod-ready) cannot
-- merge a real migration at 00357 without colliding with #990
-- when #990 merges second, so this fence bridges PR #990's 00357
-- → PR #1012's 00358 with one ADR-041 no-op. Remove this no-op
-- when PR #1012's 00358 lands, per ADR-041.
-- +goose Up
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
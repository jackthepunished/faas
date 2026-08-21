-- 00347_reserve_slot.sql — temporary concurrent-PR migration fence.
-- PR #1005 (api-contract-diff PR-A foundation) claims real slot 00347
-- (deployment_openapi_snapshots.sql). PR #1012 (stages-prod-ready)
-- cannot merge a real migration at 00347 without colliding with #1005
-- when #1005 merges second, so this fence fills the local contiguity
-- gap until #1005 lands. Remove this no-op when that migration lands,
-- per ADR-041.
--
-- PR #1012's doc-only migration has since been renumbered twice past
-- this fence (00348 → 00352) as adjacent PRs claimed 00348-00351.
-- +goose Up
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd

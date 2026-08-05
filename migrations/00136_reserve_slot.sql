-- filename: 00136_reserve_slot.sql
-- Fence at slot 136 — held by PR #654 so the migration set stays
-- contiguous 1..N where N = 139 after PR #654's renumber cascade
-- (00135 → 00137 → 00143). Slot 136 was claimed by PR #653
-- (sessions_binding) which is itself an unmerged PR; while we
-- wait for the cross-PR collision to clear, this fence bridges
-- 00135_reserve_slot and 00137_reserve_slot. PR #653's
-- `sessions_binding` shadow drops this fence on merge (mirrors
-- the `migration-gates-collision-and-replay.md` drop-on-shadow
-- pattern). Per ADR-041, the body is no-op select 1.

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

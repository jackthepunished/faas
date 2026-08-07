-- filename: 00156_reserve_slot.sql
-- Slot 00156 — bridge fence for issue #554 / PR #697 after the
-- renumber chain (155 → 157 → 156 → 157). The deployments.parked_reason
-- migration that once held slot 156 has been renumbered to 157 to
-- dodge the open PR #698 (issue-695 apps.auth_default_flip) which
-- already owns the 156 slot. This fence holds slot 156 until #698
-- lands; when #698 merges, drop this fence in the same rebase.
--
-- ADR-041 reservation pattern: the fence body is a no-op
-- `SELECT 1;` so goose applies it cleanly and writes a row in
-- goose_db_version. The fence is deleted by whichever PR first
-- lands at slot 156; do NOT back-fill this fence.

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

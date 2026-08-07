-- filename: 00161_reserve_slot.sql
-- Slot 161 — bridge fence for issue #556 / PR #732 after the
-- rebase renumber 00158 → 00159 → 00160 (PR #738 took 158 for
-- accounts_egress_allowlist_extra; PR #733 took 159 for the
-- cli-exploration replay surface). The traffic_percent migration
-- was renumbered to 160; this fence holds slot 161 until a
-- follow-up PR (PR-B for issue #556: gatewayd inter-deployment
-- picker) claims it.
--
-- ADR-041 reservation pattern: the fence body is a no-op
-- `SELECT 1;` so goose applies it cleanly and writes a row in
-- goose_db_version. The fence is deleted by whichever PR first
-- lands at slot 161; do NOT back-fill this fence.

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

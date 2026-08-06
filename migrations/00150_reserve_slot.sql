-- filename: 00150_reserve_slot.sql
-- Slot 150 — bridge fence for issue #554 / PR #673 after the
-- rebase onto main (PRs #540 / #671 / #682 consumed slots 149 /
-- 151 / 152). The liveness-probe migration was renumbered to 154
-- (see migrations/00154_deployment_liveness_probe.sql); this
-- fence holds slot 150 until a follow-up PR claims it.
--
-- ADR-041 reservation pattern: the fence body is a no-op
-- `SELECT 1;` so goose applies it cleanly and writes a row in
-- goose_db_version. The fence is deleted by whichever PR first
-- lands at slot 150; do NOT back-fill this fence.

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
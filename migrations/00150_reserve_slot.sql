-- filename: 00150_reserve_slot.sql
-- Bridge fence at slot 150 — fills the gap between PR #540's
-- 00149_webhook_deliveries / PR #673's 00150_deployment_liveness_probe
-- and PR #671's 00151_wait_until_tail (issue #667 / ADR-078).
--
-- Three open PRs (540, 673, 671) all landed on the same chain of
-- adjacent slots at the same time. The cross-PR slot gate
-- (scripts/ci/check_migration_slots.sh) caught 540↔671 at slot 149
-- and 673↔671 at slot 150; PR 671 renumbers to 151. The 149 fence
-- (00149_reserve_slot.sql) holds slot 149 until one of PR 540 or
-- PR 673 lands, and this 150 fence holds slot 150 until PR 673
-- lands. ADR-041 reservation pattern.
--
-- The fence is deleted by whichever PR first lands at slot 150 (PR
-- #673's liveness-probe migration). The 149 fence goes away when PR
-- #540 lands. Either order keeps the chain contiguous.

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

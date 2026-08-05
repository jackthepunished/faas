-- filename: 00146_reserve_slot.sql
-- Bridge fence at slot 146 — fills the gap between main's
-- 00145_sessions_binding and PR #656's 00147_deployments_scan_result
-- (issue #464 / ADR-055 / PR-B extension). Slot 146 is empty on
-- main; the fence holds the slot for now. ADR-041 carve-out.

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

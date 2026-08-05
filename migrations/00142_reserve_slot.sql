-- filename: 00142_reserve_slot.sql
-- Slot fence: bridges PR #658's app_webhook_deliveries at slot
-- 141 (or whichever slot its webhook_delivery table lands at) and
-- PR #654's apps_require_authn at slot 143. Whichever side
-- renumbers first will `git rm` this fence. ADR-041.

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

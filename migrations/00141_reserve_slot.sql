-- filename: 00141_reserve_slot.sql
-- Slot fence: PR #658 (issue #476 webhook delivery) claims slots
-- 140 + 141 on its branch (webhook tables + delivery log). This
-- fence at 141 bridges PR #658's app_webhook_deliveries at slot
-- 141 and PR #654's apps_require_authn at slot 143. Whichever of
-- PR #658 / a future PR renumbers first will `git rm` this fence.
-- The IAM mega-PR (PR #653) originally held the sessions binding-
-- hash migration at slot 141 before renumbering to 145 after the
-- cross-PR slot gate surfaced PR #658's open claim on 141.
-- ADR-041.

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

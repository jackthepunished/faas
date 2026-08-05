-- filename: 00140_reserve_slot.sql
-- Slot fence: PR #658 (issue #476 webhook delivery) claims slots
-- 140 + 141 on its branch (webhook tables + delivery log); this
-- fence at 140 is the bridge for PR #658's app_webhooks at slot
-- 140 (it gets `git rm`'d on PR #658's merge) OR — if PR #658
-- renumbers — bridges whatever PR claims slot 140 next. The
-- IAM mega-PR (PR #653) originally held the sessions binding-
-- hash migration at slot 140, then renumbered twice — to 142
-- after PR #651's open claim, then to 145 after PR #658's open
-- claim on 141 surfaced. ADR-041.

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

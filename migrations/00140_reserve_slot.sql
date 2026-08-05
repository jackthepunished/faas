-- filename: 00140_reserve_slot.sql
-- Fence at slot 140 — held by PR #654 to bridge 00139 (own fence)
-- and 00141 (PR #653's fence). PR #653 owns 140 as a fence on its
-- branch; per ADR-041 the bridge fence gets `git rm`'d on whichever
-- side merges second. Body is a no-op `select 1;` so goose applies
-- cleanly and writes a row in goose_db_version.

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

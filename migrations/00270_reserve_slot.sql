-- filename: 00270_reserve_slot.sql
-- +goose Up
-- +goose StatementBegin
-- Contiguity filler for PR #916 (issue #911 / ADR-110 PR-3a).
-- See 00268_reserve_slot.sql for the rationale. Body is a no-op
-- SELECT 1; will be shadowed by the owning PR's real DDL when
-- it merges (per ADR-041, the owning PR drops this fence in a
-- follow-up).
SELECT 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
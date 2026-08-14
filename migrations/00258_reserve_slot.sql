-- filename: 00258_reserve_slot.sql
-- +goose Up
-- +goose StatementBegin
-- Contiguity filler for PR #873 (secret-scan v2, renumbered
-- from 00233 to 00264 after PR #875 landed 00221/00222 on main;
-- ADRs 091/093/099 cluster owns 00244-00263 in their open
-- PRs #864/#895/#887). Body is a no-op SELECT 1; will be
-- shadowed by the owning PR's real DDL when it merges (per
-- ADR-041, the owning PR drops this fence in a follow-up).
SELECT 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd

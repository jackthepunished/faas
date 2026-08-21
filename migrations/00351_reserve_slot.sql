-- 00351_reserve_slot.sql — temporary concurrent-PR migration fence.
-- PR #1017 (apid_tenant_surface_cert_expiry_state) claims real
-- slot 00351. PR #1012 (stages-prod-ready) cannot merge a real
-- migration at 00351 without colliding when #1017 merges second,
-- so this fence fills the local contiguity gap until that
-- migration lands. Remove this no-op when that migration lands,
-- per ADR-041.
-- +goose Up
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
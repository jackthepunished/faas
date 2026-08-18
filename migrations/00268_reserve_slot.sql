-- filename: 00268_reserve_slot.sql
-- +goose Up
-- +goose StatementBegin
-- Contiguity filler for PR #916 (issue #911 / ADR-110 PR-3a).
-- PR-3a targets 00271 (compute_nodes release columns) +
-- 00272 (release_bundles table). Slots 00268/00269/00270 are
-- reserved by sibling PRs (#906 ADR-101 OIDC PR-A, #910 unified
-- triggers) — body is a no-op SELECT 1; so PR-3a stays
-- contiguous when those siblings land before or after PR-3a
-- merges. This fence will be reaped in a follow-up PR if
-- #906 / #910 land first and shadow the slot (per ADR-041 the
-- owning PR drops the fence in a follow-up).
SELECT 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
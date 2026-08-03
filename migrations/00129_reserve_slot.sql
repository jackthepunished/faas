-- filename: 00129_reserve_slot.sql
-- Fence at slot 129 — held by this PR so the migration set stays
-- contiguous 1..N where N = 132 (this PR's highest). Bridges the
-- gap between main's 00128_events_sidecar_name_idx.sql (real) and
-- this PR's 00131_apps_align_min_instances.sql (real, renamed from
-- 00129 due to PR #623's slot 129 claim). Without this fence the
-- embedded set has a gap at 129 (TestMigrationsContiguous fails:
-- `migration slot 129 is missing`).
--
-- ADR-041 (migration slot reservation convention): the fence body
-- is a no-op `select 1;` inside goose's StatementBegin/End markers
-- so goose applies it cleanly and writes a row in goose_db_version.
-- A later PR that wants slot 129 for a real schema shadows this
-- fence; the same PR drops this file via `git rm` so the carved-
-- out slot lands cleanly on main.
--
-- Cross-PR slot fence race (memory: migration-gates-collision-and-replay.md):
-- PR #623 (iam-6 PR-6) also claims slot 129 via
-- 00129_api_keys_org_bound.sql. Per the cross-PR fence pattern, both
-- PRs renumbered past each other or accept one another's claim. In
-- this case PR #623 landed first with its real content at 129, so
-- PR #618 renumbered to 131/132 and added this fence at 129 to keep
-- the embedded set contiguous — when PR #618 lands, this fence
-- becomes the load-bearing slot 129 entry. If main later takes
-- this fence's slot for a real migration, this file goes via
-- `git rm` in the same PR (the "drop-then-merge" pattern).

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
-- filename: 00277_reserve_slot.sql
-- +goose Up
-- +goose StatementBegin
--
-- 00277_reserve_slot.sql — reservation fence.
--
-- This slot is claimed by open PR #959 (cert engine real-mint, issue
-- #879 / ADR-100 PR-D) which introduces
-- migrations/00277_tenant_surfaces_per_host_kind.sql. The fence
-- prevents goose "duplicate version 277" collision if PR #910's
-- triggers cluster (issue #757 / ADR-100) renumbers onto 277 — and
-- gates PR #910's renumber past it (cross-PR slot precheck,
-- memory/cross-pr-slot-precheck-pr-867-collision-2026-08-13.md).
--
-- PR #910 originally occupied 00273/00274/00275 (held as fences on
-- main), then renumbered to 00277/00278/00279, and on 2026-08-18
-- the second origin/main advance collided with PR #959 (277) and
-- PR #829 (278/279/280). PR #910's final renumber moved its three
-- files to 00281/00282/00283, leaving 277 free for PR #959.
--
-- This file is a no-op. The next migration PR to claim 00277 will
-- collide with this fence via TestMigrationsContiguous — the
-- cross-PR slot precheck gates this at PR-open time. Once PR #959
-- lands, this fence will be removed by a follow-up rebase commit.
-- See memory/cross-pr-slot-fence-pagination-gate.md for the broader
-- pattern.

select 1;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

select 1;

-- +goose StatementEnd
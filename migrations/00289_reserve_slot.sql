-- filename: 00289_reserve_slot.sql
-- +goose Up
-- +goose StatementBegin
--
-- 00289_reserve_slot.sql — reservation fence.
--
-- This slot is held by PR #910 (feat(triggers): unified
-- event-source-mapping primitive — issue #757 / ADR-100) so the
-- triggers-mega migration chain can step past main's
-- 00287_pg_ratelimit_add_rule_scope.sql (PR #963, issue #881
-- Phase 3 per-consumer). On PR #910's branch the actual migration
-- at 00289 renumbered upward to 00292_triggers_poison_strategy.sql
-- to dodge this fence; the fence remains a no-op so
-- TestMigrationsContiguous sees a gap-free 287..292 sequence on
-- the triggers-mega branch. This file is a no-op; it stays as
-- reservation metadata until PR #910's renumber chain settles.

select 1;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

select 1;

-- +goose StatementEnd
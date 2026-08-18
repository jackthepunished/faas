-- filename: 00285_reserve_slot.sql
-- +goose Up
-- +goose StatementBegin
--
-- 00285_reserve_slot.sql — reservation fence.
--
-- This slot is claimed by open PR #963 (issue #881 Phase 4 — rate-
-- limit improvements mega-PR — central mode + X-RouteRateLimit-Policy
-- + dry-run), which introduces migrations/00285_pg_ratelimit_add_rule_scope.sql
-- on its own branch. PR #964 (data_upstreams PK widens to include
-- deployment_scope, issue #954) renumbered its migration to 00286
-- (above PR #963's 00285) but needed a fence here so
-- TestMigrationsContiguous sees a gap-free 285..286 sequence on
-- PR #964's branch. This file is a no-op; the actual migration
-- lands when PR #963 merges.

select 1;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

select 1;

-- +goose StatementEnd

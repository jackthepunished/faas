-- filename: 00286_reserve_slot.sql
-- +goose Up
-- +goose StatementBegin
--
-- 00286_reserve_slot.sql — reservation fence.
--
-- This slot is claimed by open PR #964 (data-upstreams PK widening
-- cluster), which holds its own 00286 widening on its branch
-- (release/issue-954-data-upstreams-deployment-scope, head
-- 7f5331fad at this writing). The fence backfills the slot left
-- empty on this branch by renaming the ADR-104 amendment 5 schema
-- (issue #881 Phase 4 widening) from 00286 to 00287 to dodge the
-- cross-PR collision. See 00281_reserve_slot.sql for the cross-PR
-- slot precheck pattern.
--
-- This file is a no-op; the real schema lands at
-- 00287_pg_ratelimit_add_rule_scope.sql on this branch. PR #964
-- will drop this fence when it merges its 00286 schema.

select 1;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

select 1;

-- +goose StatementEnd

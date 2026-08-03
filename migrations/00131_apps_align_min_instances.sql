-- filename: 00131_apps_align_min_instances.sql
-- +goose Up

-- ADR-071 §Downstream / issue #557 closure cleanup. Project the
-- legacy `apps.min_instances` column into the canonical
-- `apps.scaling_policy->>'min_instances'` jsonb on rows where the
-- column is strictly greater than the jsonb. The inverse direction
-- (jsonb > column) is left untouched on purpose — the jsonb is the
-- customer's explicit PATCH intent and a silent rewrite would clobber
-- a configuration they set via the SetScalingPolicy path.
--
-- Post-migration invariant: `EffectiveMinInstances() ==
-- COALESCE(jsonb.min_instances, column)` for every app row. Pre-#557
-- the two sources could diverge (a bare SetAppMinInstances PATCH
-- writes only the column; pkg/state/pgstore.go keepMinInstancesInSync
-- only re-projects when the jsonb path writes). The helper
-- (`pkg/state/app_min_instances_effective.go`) already returns
-- `max(column, jsonb)`, so divergent rows behaved correctly the
-- moment the helper shipped; this migration collapses the divergence
-- so a future metric or wire shape that reads the jsonb directly
-- agrees with the helper.
--
-- The UPDATE is wrapped in a single SET to keep the row rewrite
-- atomic; a per-row trigger isn't needed because the helper is the
-- only read-side path and it always returns max(). Predicate is
-- strict (col > jsonb) so zero-floor rows are no-ops.
--
-- Replay-safe (PR #377 / ADR-041): the WHERE clause filters on the
-- existing divergence, so a second MigrateUp against an aligned
-- schema is a clean no-op. No index needed (UPDATE touches every
-- divergent row at most once; the divergence count is bounded by the
-- number of bare SetAppMinInstances PATCHes that did not flow through
-- SetScalingPolicy — a one-time cleanup).
-- +goose StatementBegin

UPDATE apps
SET scaling_policy = jsonb_set(
    COALESCE(scaling_policy, '{}'::jsonb),
    '{min_instances}',
    to_jsonb(min_instances),
    false -- createIfMissing
)
WHERE min_instances > 0
  AND COALESCE((scaling_policy->>'min_instances')::int, 0) < min_instances;

-- +goose StatementEnd

-- +goose Down
-- Forward-only. The pre-#557 divergence was a bug, not a feature;
-- rolling back this migration by zeroing the jsonb would silently
-- revoke the customer's floor on every app where the legacy column
-- is the authoritative source. ADR-071 §Downstream keeps the inverse
-- direction (jsonb > column) untouched on purpose for the same
-- reason. Down is a no-op so an operator-driven downgrade preserves
-- data.
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
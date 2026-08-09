-- filename: 00162_deployments_live_traffic_idx.sql
-- +goose Up
-- +goose StatementBegin
-- Issue #556 / PR-B: partial index that backs the gateway's
-- LiveDeployments(appID) plural query. The picker reads every
-- live deployment of one app on every NotifyDeploymentChanged
-- (typically ~1/s during a rollout) so the access path must be
-- index-only and the predicate must be partial — the typical app
-- has one live deployment, not the O(N) of all rows.
--
-- Why partial: traffic_percent and id are only meaningful for
-- status='live'; superseded / failed / parked rows are picked up
-- by other queries (ListDeploymentsForApp). Including them in the
-- index would bloat it for no benefit.
--
-- Why INCLUDE (traffic_percent, id): makes LiveDeployments an
-- Index Only Scan — the planner never touches the heap. Without
-- INCLUDE the planner would have to fetch every tuple to read
-- the two columns and the cost would scale with row width.
--
-- Why here and not in 00160: PR-A deliberately kept its blast
-- radius small (see migrations/00160 line 29-32). PR-B is the
-- gateway surface that issues this query, so this index is the
-- right home for it.
--
-- Slot 162 is the next free slot on origin/main (slot 161 was a
-- reserve fence that PR-B deletes; 162 keeps a one-slot gap for
-- any in-flight sibling PR claiming 161 per ADR-041).
--
-- Replay-safe (ADR-041): CREATE INDEX IF NOT EXISTS makes a second
-- MigrateUp a no-op; the apply_walk_test pins this at the
-- directory level and the per-migration test below is defence in
-- depth.
CREATE INDEX IF NOT EXISTS deployments_live_traffic_idx
  ON deployments (app_id)
  INCLUDE (traffic_percent, id)
  WHERE status = 'live';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS deployments_live_traffic_idx;
-- +goose StatementEnd
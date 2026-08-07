-- filename: 00159_deployments_traffic_percent.sql
-- +goose Up
-- +goose StatementBegin
-- issue #556 / traffic splitting across deployments (PR-A: schema +
-- validation + wire surface only; no picker mutation).
--
-- PR-A wires the persistence layer so every deployment row carries
-- a traffic_percent in [0, 100] and the live-row sum is 100 by
-- construction. The gateway still routes 100% to the most-recent
-- live row (today's behaviour); the inter-deployment picker lands
-- in PR-B.
--
-- Conventions (matching the load-bearing precedents in this repo):
--
--   * NOT NULL DEFAULT 100 — existing rows land at 100 (they were
--     100% anyway because there was no splitting). The supersede
--     step in CreateDeployment (pkg/state/pgstore.go:3626-3699)
--     stamps the prior row at 0 inside the same transaction so
--     Σ = 100 trivially for the 1-live-deployment case.
--
--   * CHECK (traffic_percent BETWEEN 0 AND 100) — the row-level
--     guard. App-level Σ = 100 is enforced by the
--     UpdateDeploymentTraffic transaction in pkg/state/pgstore.go
--     (the BEGIN/SELECT-FOR-UPDATE/UPDATE/UPDATE/CHECK/COMMIT
--     block), not by a DEFERRABLE trigger — the codebase has no
--     precedent for deferred constraints and PR-A is plumbing, not
--     architecture.
--
--   * Partial index `deployments_live_traffic_idx ON deployments(app_id)
--     WHERE status='live'` is NOT added in PR-A. PR-B's
--     LiveDeployments(appID) plural query will need it; deferring
--     keeps this migration's blast radius small.
--
-- No data backfill is required — DEFAULT 100 covers every existing
-- row, including those pre-dating the feature (build failures,
-- superseded rows, parked rows). The idempotent UPDATE below is a
-- defensive tripwire in case a future refactor swaps the DEFAULT
-- for something looser.
ALTER TABLE deployments
  ADD COLUMN IF NOT EXISTS traffic_percent int NOT NULL DEFAULT 100;

ALTER TABLE deployments
  DROP CONSTRAINT IF EXISTS deployments_traffic_percent_chk;
ALTER TABLE deployments
  ADD CONSTRAINT deployments_traffic_percent_chk
  CHECK (traffic_percent BETWEEN 0 AND 100);

-- Idempotent backfill tripwire. NOT NULL DEFAULT 100 makes this a
-- no-op, but it survives future refactors that flip the DEFAULT
-- (defence in depth — the Σ = 100 invariant is enforced by the
-- state-layer transaction, not by this backfill).
UPDATE deployments SET traffic_percent = 100 WHERE traffic_percent IS NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE deployments
  DROP CONSTRAINT IF EXISTS deployments_traffic_percent_chk;
ALTER TABLE deployments
  DROP COLUMN IF EXISTS traffic_percent;
-- +goose StatementEnd
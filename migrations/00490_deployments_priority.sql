-- +goose Up
-- +goose StatementBegin
-- filename: 00490_deployments_priority.sql
-- ADR-124 deployment queue controls — M3: priority column on deployments.
--
-- Reorder + deploy-immediately surface. `priority` is an int in [0,1000]
-- where lower numbers run first. Default = 100 (current behaviour, FIFO).
-- The 0..1000 range is wide enough for a "deploy immediately" bump (0)
-- through "background rebuild" (1000) without colliding with the
-- existing fairness ordering.
--
-- The partial index covers the only claim path that matters:
--   WHERE status = 'pending' ORDER BY priority ASC, created_at ASC
-- which builderd reads when the dashboard's "deploy-immediately" button
-- bumps a queued row's priority. Existing FIFO claimers that ignore
-- priority still work — the index is a prefix and the (priority,
-- created_at) tuple orders correctly when priorities are equal.
-- (deployed_at and enqueued_at don't exist on deployments; created_at
-- is the FIFO tiebreaker column. This surfaced in CI round-6.)
--
-- reordered_at + reordered_by_principal back the audit trail of the
-- last reorder on this row (pgstore.ReorderDeployment writes them).
-- Empty values mean "never reordered". A future PR could enforce a
-- minimum-time-since-last-reorder rate limit on top of these columns;
-- for now they're observation data only.
ALTER TABLE deployments
  ADD COLUMN IF NOT EXISTS priority int NOT NULL DEFAULT 100;

ALTER TABLE deployments
  ADD COLUMN IF NOT EXISTS reordered_at timestamptz;
ALTER TABLE deployments
  ADD COLUMN IF NOT EXISTS reordered_by_principal text;

ALTER TABLE deployments
  DROP CONSTRAINT IF EXISTS deployments_priority_check;
ALTER TABLE deployments
  ADD CONSTRAINT deployments_priority_check
  CHECK (priority BETWEEN 0 AND 1000);

CREATE INDEX IF NOT EXISTS deployments_pending_priority_idx
  ON deployments (app_id, priority, created_at)
  WHERE status = 'pending';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS deployments_pending_priority_idx;
ALTER TABLE deployments
  DROP CONSTRAINT IF EXISTS deployments_priority_check;
ALTER TABLE deployments
  DROP COLUMN IF EXISTS reordered_by_principal;
ALTER TABLE deployments
  DROP COLUMN IF EXISTS reordered_at;
ALTER TABLE deployments
  DROP COLUMN IF EXISTS priority;
-- +goose StatementEnd

-- +goose Up
-- +goose StatementBegin
-- issue #554 / ADR-079 (liveness probe, restart on wedged VM)
--
-- AC #3 / AC #5 follow-up. PR #673 emitted the per-park audit row
-- (`instances.parked_liveness_exhausted`) and flipped apps.status to
-- `evicted_cold`, but did not persist a per-deployment parking reason.
-- This migration closes the customer-visible surface — GET /v1/apps/{slug}
-- — by adding `deployments.parked_reason` + `deployments.parked_at` so
-- the apid handler can render `parked_deployment: { id, parked_reason,
-- parked_at }`.
--
-- Closed-set CHECK constraint on parked_reason pins the audit taxonomy:
--   * liveness_exhausted — engine.ParkDeployment after the liveness
--                          window (issue #554)
--   * lifecycle_park     — admin / parkApp handler (cmd/apid/handlers_ext.go)
--   * admin_park         — reserved for an operator-driven park path
--                          (e.g. compliance hold); not wired yet.
--
-- additive + nullable: existing rows land with NULL parked_reason, NULL
-- parked_at. Per ADR-041 (slot discipline), this slot is 155 — a free
-- slot at the time of authoring; pre-flight
-- `git ls-tree origin/main migrations/ | grep '^155'` was empty.
ALTER TABLE deployments
  ADD COLUMN IF NOT EXISTS parked_reason text;
ALTER TABLE deployments
  ADD COLUMN IF NOT EXISTS parked_at timestamptz;

ALTER TABLE deployments
  DROP CONSTRAINT IF EXISTS deployments_parked_reason_check;
ALTER TABLE deployments
  ADD CONSTRAINT deployments_parked_reason_check
  CHECK (parked_reason IS NULL OR parked_reason IN ('liveness_exhausted', 'lifecycle_park', 'admin_park'));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE deployments
  DROP CONSTRAINT IF EXISTS deployments_parked_reason_check;
ALTER TABLE deployments
  DROP COLUMN IF EXISTS parked_at;
ALTER TABLE deployments
  DROP COLUMN IF EXISTS parked_reason;
-- +goose StatementEnd

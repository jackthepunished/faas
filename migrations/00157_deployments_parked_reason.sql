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
-- parked_at. Per ADR-041 (slot discipline), this slot is 157. The
-- renumber chain (155 → 157 → 156 → 157) closed out after PR #698
-- (issue-695 apps.auth_default_flip) merged into main at 16:21:08
-- with 00156_apps_auth_default_flip.sql — the 156 slot became a real
-- main-landed migration and PR #697 picks 157 as the next free slot
-- above main's head.
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

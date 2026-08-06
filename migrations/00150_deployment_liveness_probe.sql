-- +goose Up
-- +goose StatementBegin
-- filename: 00150_deployment_liveness_probe.sql
-- Slot 150 — issue #554 / ADR-078 follow-up: the per-deployment
-- liveness-probe override column on `deployments`. The fence at
-- slot 149 (migrations/00149_reserve_slot.sql) reserved this slot
-- for "if a follow-up lands `deployments.parked_reason text` for
-- AC #3 of issue #554"; instead of parked_reason, v1 needs
-- override_liveness_probe so the customer's PATCH /v1/apps/{slug}
-- /deployment liveness tuning persists across recreates.
--
-- Why override_liveness_probe jsonb (not text):
--   * The deployment overrides table already uses jsonb for the
--     sibling override_healthcheck (migrations/00079_deployment_overrides.sql).
--     Matching the pattern keeps the read-side coalesce + scanDeployment
--     wrapper consistent — adding a sibling JSONB column is one new
--     $N in CreateDeployment + one new column in deploymentSelectColumns.
--   * The structured shape ({path, interval_s, timeout_s,
--     consecutive_failures}) is what the vmmd liveness_recv goroutine
--     consumes (cmd/vmmd/liveness_recv.go::livenessProbeConfig).
--     Storing as text would force a parser on the read path.
--
-- This migration is additive + nullable + coalesce-defaults to NULL
-- so existing rows round-trip cleanly. The applications/read path
-- (scanDeployment) uses coalesce(override_liveness_probe, '{}'::jsonb)
-- on the SELECT side, identical to override_healthcheck.

ALTER TABLE deployments
  ADD COLUMN IF NOT EXISTS override_liveness_probe jsonb;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE deployments
  DROP COLUMN IF EXISTS override_liveness_probe;
-- +goose StatementEnd
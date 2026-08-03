-- filename: 00133_deployments_min_instances.sql
-- +goose Up
-- +goose StatementBegin

-- Issue #557 closure / ADR-072 — per-deployment min_instances axis.
-- Adds the `deployments.min_instances` column mirroring the existing
-- `apps.min_instances` column. Default 0 = "inherit from parent
-- app" (the inheritance default; ADR-072 §Decision 1). The per-plan
-- cap is enforced at the apid PATCH handler against
-- api.Plan.MaxMinInstances; the DB CHECK is a belt-and-suspenders
-- bound (100 is the highest per-plan cap across all four plans —
-- Scale = 10, so 100 leaves room for future plan increases without
-- a second migration).
--
-- The column lands in this PR (no longer a follow-up). pgstore
-- references the column at the CreateDeployment INSERT and the
-- UpdateDeploymentMinInstances UPDATE; without this migration those
-- statements 42703 in CI.
--
-- Why int and not jsonb: `apps.min_instances` is int (legacy), and
-- deployments have no scaling_policy jsonb — the column is the
-- single source of truth, and the EffectiveMinInstances() helper
-- returns max(app, deployment) at the trigger + reaper + meterd
-- sites. ADR-071 §Decision 2 reads the effective floor from the
-- column directly; the jsonb mirror on apps was an artifact of the
-- PATCH route shape (issue #471 streaming + #470 warm snapshot both
-- write jsonb) and is not needed on deployments — the PATCH route
-- writes a single int.
--
-- Replay-safe (PR #377 / ADR-041): ADD COLUMN IF NOT EXISTS +
-- ADD CONSTRAINT IF NOT EXISTS. A second MigrateUp is a no-op.

ALTER TABLE deployments
    ADD COLUMN IF NOT EXISTS min_instances int NOT NULL DEFAULT 0;

ALTER TABLE deployments
    ADD CONSTRAINT IF NOT EXISTS deployments_min_instances_chk
    CHECK (min_instances >= 0 AND min_instances <= 100);

-- +goose StatementEnd

-- +goose Down
-- Forward-only (ADR-071 §Downstream convention). Dropping the column
-- would silently zero every per-deployment floor on a downgrade,
-- waking no instances on a customer's "Scale with min_instances=5
-- per deployment" until they re-PATCH — a billing-and-SLA surprise.
-- Down is a no-op so an operator-driven downgrade preserves data.
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
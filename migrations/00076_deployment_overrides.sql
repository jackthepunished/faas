-- filename: 00076_deployment_overrides.sql
-- +goose Up
-- +goose StatementBegin
-- Add deploy-time override columns to `deployments` (issue #460 /
-- ADR-053). Customers redeploy the same digest-pinned OCI image with a
-- different entrypoint/cmd/env/port/healthcheck without rebuilding the
-- image; the override is per-DEPLOYMENT (not per-app) so re-deploying
-- the same image with a different port is a normal flow.
--
-- Six columns:
--   override_entrypoint    text[]   — full argv to exec; replaces the
--                                    image-derived entrypoint
--   override_cmd          text[]   — appended argv; replaces the
--                                    image-derived cmd
--   override_env          jsonb    — env map to merge into image env
--                                    (override wins on key collision)
--   override_env_secrets  jsonb    — sealed-secret refs ("secret:NAME")
--                                    resolved at wake against the
--                                    existing app_secrets store
--   override_port         int      — listen port (1..65535, 0 = absent)
--   override_healthcheck  jsonb    — {path, interval_s, timeout_s, retries};
--                                    persisted in PR-A; the actual HTTP
--                                    probe in vmm.waitReady is a follow-up
--
-- Replay-safe (PR #377 / ADR-041): ADD COLUMN IF NOT EXISTS. The columns
-- are nullable and the apid handler only writes values that have passed
-- CreateDeploymentOverrides.Validate, so the jsonb columns can accept any
-- JSON shape at the DB layer — validation lives in apid, not in the DB.
--
-- No new limits columns on pkg/api/limits.go: env + env_secrets share
-- Limits.EnvVarsMax (ADR-045 §Decision 1). A per-deploy quota would let
-- a customer bypass the per-app quota by issuing many deploys.
-- +goose StatementEnd
ALTER TABLE deployments
  ADD COLUMN IF NOT EXISTS override_entrypoint    text[],
  ADD COLUMN IF NOT EXISTS override_cmd          text[],
  ADD COLUMN IF NOT EXISTS override_env          jsonb,
  ADD COLUMN IF NOT EXISTS override_env_secrets  jsonb,
  ADD COLUMN IF NOT EXISTS override_port         int,
  ADD COLUMN IF NOT EXISTS override_healthcheck  jsonb;

-- +goose Down
-- +goose StatementBegin
-- Reverse: drop the six columns. A row that wrote overrides under the
-- new columns will lose them on downgrade (jsonb/text[] are opaque to
-- the down-migration); the GET /v1/apps/{slug}/deployments/{id} response
-- shape omits the override fields because the columns no longer exist,
-- which is the correct degraded behaviour.
ALTER TABLE deployments
  DROP COLUMN IF EXISTS override_entrypoint,
  DROP COLUMN IF EXISTS override_cmd,
  DROP COLUMN IF EXISTS override_env,
  DROP COLUMN IF EXISTS override_env_secrets,
  DROP COLUMN IF EXISTS override_port,
  DROP COLUMN IF EXISTS override_healthcheck;
-- +goose StatementEnd

/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * Readiness-probe shape on the deploy-time override object (issue #460 /
 * ADR-053). Today the probe stays a bare TCP accept — `path`, `interval_s`,
 * `timeout_s`, `retries` are persisted but not yet exercised by `vmm.waitReady`.
 *
 * Validation rules (enforced in `pkg/api/dto.go::CreateDeploymentOverrides.Validate`):
 * - `path` must start with `/`.
 * - `interval_s`, `timeout_s`, `retries` must be `>= 0`.
 * - Missing fields default to 0 (interpreted as "use image default" by the
 * future probe implementation).
 *
 * M-1 (ADR-136) widens additively with `test` (argv of the OCI HEALTHCHECK
 * command) and `start_period_s` (Docker 17.05+ startup grace). Runtime
 * wiring lands in M-2 (ADR-X5).
 *
 */
export type DeploymentHealthcheck = {
  /**
   * Path the probe requests from the guest; must start with `/` (e.g. `/healthz`).
   */
  path: string;
  /**
   * Probe interval in seconds; 0 = use image default.
   */
  interval_s?: number;
  /**
   * Probe timeout in seconds; 0 = use image default.
   */
  timeout_s?: number;
  /**
   * Consecutive failures before the instance is considered unhealthy; 0 = use image default.
   */
  retries?: number;
  /**
   * Argv of the OCI HEALTHCHECK command, prefixed by "CMD", "CMD-SHELL", or "NONE". Surfaces onto AppManifest.Healthcheck.Test at apply_overrides time.
   */
  test?: Array<string>;
  /**
   * Startup grace during which probe failures don't count (Docker 17.05+, default 0s). Surfaces onto AppManifest.Healthcheck.StartPeriodS.
   */
  start_period_s?: number;
};


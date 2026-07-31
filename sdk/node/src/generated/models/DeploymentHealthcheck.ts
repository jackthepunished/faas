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
};


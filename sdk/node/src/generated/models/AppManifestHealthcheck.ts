/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * AppManifest-level projection of the OCI HEALTHCHECK shape (ADR-136 §Decision 3-4). Durations are integer seconds at the JSON boundary to match OCI/Docker conventions. Runtime polling lands in M-2 (ADR-X5); M-1 surfaces the field for the registry-pull path.
 */
export type AppManifestHealthcheck = {
  /**
   * Argv of the check command, prefixed by "CMD", "CMD-SHELL", or "NONE" per Docker semantics.
   */
  test?: Array<string>;
  /**
   * Poll cadence after StartPeriodS elapses (Docker default 30s).
   */
  interval_s?: number | null;
  /**
   * Per-probe exec timeout (Docker default 30s).
   */
  timeout_s?: number | null;
  /**
   * Consecutive failure count to mark unhealthy (Docker default 3).
   */
  retries?: number | null;
  /**
   * Startup grace during which failures don't count (Docker 17.05+, default 0s).
   */
  start_period_s?: number | null;
};


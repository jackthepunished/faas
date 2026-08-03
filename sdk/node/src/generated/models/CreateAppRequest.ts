/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * App creation payload: slug, type (app|function), runtime (only for function), RAM MB, max concurrency, idle timeout, and optional manifest.
 */
export type CreateAppRequest = {
  slug: string;
  type?: 'app' | 'function';
  runtime?: 'node22' | 'python312' | 'go124' | 'go124-alpine' | 'node24' | 'python313';
  ram_mb?: number;
  max_concurrency?: number;
  idle_timeout_s?: number;
  /**
   * Per-app streaming flag. Omitted at create-time → apid applies the plan default (issue #471).
   */
  streaming_enabled?: boolean;
  /**
   * Per-app two-tier snapshot flag (issue #470 / ADR-055). Omitted at create-time → apid applies the plan default. Free/Hobby PATCH-true is rejected.
   */
  warm_snapshot_enabled?: boolean;
  /**
   * Optional create-time override for the warm-tier request-count threshold (issue #470 / ADR-055). Range [1, 100]. Omitted → apid applies the plan default.
   */
  warm_snapshot_min_requests?: number;
  /**
   * Optional create-time override for the warm-tier time-since-first-ready threshold, milliseconds (issue #470 / ADR-055). Range [100, 60000]. Omitted → apid applies the plan default.
   */
  warm_snapshot_min_ms?: number;
};


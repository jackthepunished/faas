/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { ScalingPolicy } from './ScalingPolicy.js';
/**
 * Partial update — every field is optional; omitted fields are unchanged.
 */
export type UpdateAppRequest = {
  ram_mb?: number | null;
  idle_timeout_s?: number | null;
  max_concurrency?: number | null;
  min_instances?: number | null;
  /**
   * v4 or v6 CIDR allowlist; empty array clears to chain-default-accept.
   */
  egress_allowlist?: Array<string>;
  /**
   * Per-instance RPS target for the reactive scale-up trigger. 0 = disable. Hobby/Pro/Scale only. Values < 0 are 422 invalid_autoscale_target_rps.
   */
  autoscale_target_rps?: number | null;
  /**
   * Per-instance CPU% target (1..100, 0 = disable) for the reactive scale-up trigger. Pro/Scale only. Values outside [1, 100] (other than 0) are 422 invalid_autoscale_target_cpu_pct.
   */
  autoscale_target_cpu_pct?: number | null;
  /**
   * Per-app streaming flag (issue #471). Omitted → no change. Free PATCHing true is 403 plan_streaming_not_allowed.
   */
  streaming_enabled?: boolean | null;
  /**
   * Per-app scaling policy. Omitted → no change. Non-null → atomic full-overwrite of the jsonb column.
   */
  scaling_policy?: (null | ScalingPolicy);
  /**
   * DEPRECATED on this surface. The customer PATCH /v1/apps/{slug} endpoint silently drops require_signed; the operator endpoint PATCH /v1/apps/{slug}/security is the only path that flips the flag (issue #472 / ADR-054). The field is parsed for backwards compatibility but never persisted from this endpoint.
   */
  require_signed?: boolean | null;
  /**
   * Per-app two-tier snapshot flag (issue #470 / ADR-055). Omitted → no change. PATCH-true on Free/Hobby is rejected with 403 plan_warm_snapshot_not_allowed.
   */
  warm_snapshot_enabled?: boolean | null;
  /**
   * Per-app request-count threshold for warm-tier capture (issue #470 / ADR-055). Range [1, 100]. Omitted → no change.
   */
  warm_snapshot_min_requests?: number | null;
  /**
   * Per-app time-since-first-ready threshold for warm-tier capture, milliseconds (issue #470 / ADR-055). Range [100, 60000]. Omitted → no change.
   */
  warm_snapshot_min_ms?: number | null;
};


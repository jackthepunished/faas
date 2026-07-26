/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * Per-app usage for one month: GB-hours consumed, request count, and an informational CPU-µs field (issue #279 / PR-B). The CPU dimension is observable but not yet billed.
 */
export type UsageResponse = {
  app_id: string;
  mb_seconds: number;
  requests: number;
  included_gb_hours: number;
  /**
   * Cumulative host cgroup CPU-µs (informational; not billed). issue #279 / PR-B.
   */
  cpu_usec?: number;
};


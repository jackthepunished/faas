/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * Account-level monthly roll-up: included GB-hours, used, overage math, remaining balance, and informational used_cpu_hours (issue #279 / PR-B). The CPU dimension is observable but not yet billed; the GB-hours fields drive the overage math.
 */
export type UsageSummaryResponse = {
  month: string;
  used_gb_hours: number;
  included_gb_hours: number;
  overage_gb_hours: number;
  /**
   * Integer cents. Overages are €0.01/GB-h.
   */
  overage_cents: number;
  /**
   * Per-month CPU-hours (informational; not billed). issue #279 / PR-B.
   */
  used_cpu_hours?: number;
};


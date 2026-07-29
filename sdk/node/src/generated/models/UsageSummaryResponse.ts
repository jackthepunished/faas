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
  /**
   * Per-month egress GB (informational; not billed). Σ tx_bytes + net_tx_bytes across all apps, converted to GB. ADR-046.
   */
  used_egress_gb?: number;
  /**
   * Per-month ingress GB (informational; not billed). Σ net_rx_bytes across all apps, converted to GB. ADR-048. Mirror of `used_egress_gb` for the inbound direction.
   */
  used_ingress_gb?: number;
  /**
   * Per-month sum of WAKE_RESTORE→WAKE_COLD_BOOT transitions across every app on the account (informational; not billed). ADR-048.
   */
  cold_boots?: number;
};


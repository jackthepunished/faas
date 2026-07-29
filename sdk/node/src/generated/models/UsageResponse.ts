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
  /**
   * Per-app monthly HTTP response bytes the gateway forwarded (informational; not billed). ADR-046. The gateway-side producer lands in PR-2; until then this field stays 0. The future egress-billing PR picks the unit; this field reports interface bytes (includes Ethernet framing).
   */
  tx_bytes?: number;
  /**
   * Per-app monthly byte delta on root-side vethHost.rx_bytes (informational; not billed). ADR-046. Sourced from vmmd netstats.Cache via schedd ListInstanceStats. Includes Ethernet framing — same kernel counter the per-plan tc tbf qdisc reads, so the cap and the meter are consistent.
   */
  net_tx_bytes?: number;
};


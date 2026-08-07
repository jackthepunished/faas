/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { SLODuration } from './SLODuration.js';
/**
 * Flat account-wide SLO rollup returned by
 * `GET /v1/account/slo?window=` (issue #696 / ADR-082).
 * Mirrors `AppSLOResponse` field-for-field except for
 * `app_id` / `app_slug` (the rollup is account-wide). The
 * fields are scalar sums/rates across the account; per-app
 * drill-down is served by the existing `/v1/apps/metrics`
 * endpoint.
 *
 * `source` follows the same `"degraded: <reason>"` contract
 * as the per-app endpoint.
 *
 */
export type AccountSLOResponse = {
  window: '1h' | '24h' | '7d';
  /**
   * "prometheus" on success; "degraded: <reason>" otherwise.
   */
  source: string;
  as_of: string;
  request_duration: SLODuration;
  error_rate_pct: number;
  cold_boot_rate_pct: number;
  /**
   * Sum across all apps for the account.
   */
  instance_hours: number;
  /**
   * Sum across all apps for the account.
   */
  gb_hours: number;
  wake_queue_p95_ms: number;
  requests_total: number;
  throttled_total: number;
};


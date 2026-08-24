/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { DebugTelemetryRequestItem } from './DebugTelemetryRequestItem.js';
/**
 * Response from GET /v1/apps/{slug}/debug/requests (ADR-127).
 * `since` echoes the effective window used (after the plan's
 * `DebugTelemetryRetentionDays` clamp) so the dashboard can
 * surface a "you widened past the cap" tile.
 *
 */
export type DebugTelemetryListResponse = {
  /**
   * Effective window applied (e.g. '24h', '72h').
   */
  since: string;
  requests: Array<DebugTelemetryRequestItem>;
};


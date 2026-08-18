/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * One row of the dry-run preview (ADR-104 amendment 5,
 * issue #881 Phase 4 D1/D2). For each surviving route we
 * report the count of sub-windows where the observed rate
 * exceeded the candidate rps — a count of "would-have-
 * rejected" requests over the window.
 *
 */
export type ThrottlePreviewRow = {
  route: string;
  /**
   * Echo of the candidate rps the preview evaluated against.
   */
  candidate_rps: number;
  /**
   * Count of sub-windows in the recommendation window
   * where observed rps exceeded the candidate. NaN/Inf
   * from Prometheus are coerced to 0 via
   * `pkg/appmetrics.SafeFloat`.
   *
   */
  over_cap_count: number;
  /**
   * RFC 3339 UTC window-start the preview was evaluated against.
   */
  window_start: string;
  /**
   * RFC 3339 UTC window-end the preview was evaluated against.
   */
  window_end: string;
};


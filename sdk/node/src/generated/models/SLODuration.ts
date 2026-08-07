/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * Shared latency sub-shape used by `AppSLOResponse` and
 * `AccountSLOResponse`. Three percentiles over the SLO
 * window (2xx class only); NaN/Inf from `histogram_quantile`
 * on an empty window is coerced to 0 by the handler.
 *
 */
export type SLODuration = {
  /**
   * p50 latency in milliseconds.
   */
  p50_ms: number;
  /**
   * p95 latency in milliseconds.
   */
  p95_ms: number;
  /**
   * p99 latency in milliseconds.
   */
  p99_ms: number;
};


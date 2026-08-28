/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * Per-route latency stats for one side of the compare.
 */
export type DebugCompareRouteStats = {
  route: string;
  source_p50_ms?: number | null;
  source_p95_ms?: number | null;
  source_p99_ms?: number | null;
  source_count?: number | null;
  mirror_p50_ms?: number | null;
  mirror_p95_ms?: number | null;
  mirror_p99_ms?: number | null;
  mirror_count?: number | null;
};


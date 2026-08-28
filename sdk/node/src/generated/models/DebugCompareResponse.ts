/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { DebugCompareRouteStats } from './DebugCompareRouteStats.js';
/**
 * POST response from /v1/apps/{slug}/debug/compare (ADR-127 / PR-B).
 */
export type DebugCompareResponse = {
  source: string;
  mirror: string;
  routes: Array<DebugCompareRouteStats>;
};


/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { MirrorRuleResponse } from './MirrorRuleResponse.js';
/**
 * Wrapper for GET /v1/apps/{slug}/mirrors. Bounded by
 * `Limits.MirrorTargetsPerApp` (1-3) — no cursor in A2.
 *
 */
export type MirrorRuleListResponse = {
  rules: Array<MirrorRuleResponse>;
  count: number;
};


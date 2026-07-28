/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { AppEnvResponse } from './AppEnvResponse.js';
/**
 * List of env var envelopes (no plaintext values).
 */
export type AppEnvListResponse = {
  env: Array<AppEnvResponse>;
  quota_max: number;
  count: number;
};


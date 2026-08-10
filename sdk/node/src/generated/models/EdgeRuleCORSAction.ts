/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * Stamps CORS headers + handles preflight in-process.
 */
export type EdgeRuleCORSAction = {
  allow_origins: Array<string>;
  allow_methods: Array<string>;
  allow_headers?: Array<string>;
  expose_headers?: Array<string>;
  allow_credentials?: boolean;
  max_age_seconds?: number;
};


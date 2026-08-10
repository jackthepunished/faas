/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * 3xx short-circuit.
 */
export type EdgeRuleRedirectAction = {
  status_code: 301 | 302 | 307 | 308;
  to: string;
  /**
   * Headers stamped on the redirect response.
   */
  headers?: Record<string, string>;
};


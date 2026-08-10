/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * Validates an inbound Bearer JWT against a JWKS endpoint.
 */
export type EdgeRuleJWTAction = {
  issuer: string;
  audience?: Array<string>;
  jwks_url: string;
  algorithms: Array<'RS256' | 'RS384' | 'RS512' | 'ES256' | 'ES384' | 'ES512' | 'HS256' | 'HS384' | 'HS512'>;
  required_claims?: Record<string, string>;
};


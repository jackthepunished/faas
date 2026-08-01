/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { TrustedSigner } from './TrustedSigner.js';
/**
 * GET /v1/apps/{slug}/trusted_signers response body. Empty list is the EXPECTED state for any app with require_signed=false.
 */
export type AppTrustedSignerListResponse = {
  signers: Array<TrustedSigner>;
};


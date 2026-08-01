/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * PATCH body for `/v1/apps/{slug}/security`. `require_signed` is a
 * pointer so the wire form can distinguish "don't touch" (nil) from
 * "explicit true/false" — the same Set-bit convention the broader
 * UpdateAppRequest uses (issue #471 streaming flag precedent).
 *
 */
export type AppSecurityRequest = {
  /**
   * Operator-only toggle. nil = no change. *true = opt in to signature enforcement (requires the trust list to be non-empty). *false = opt out.
   */
  require_signed?: boolean | null;
};


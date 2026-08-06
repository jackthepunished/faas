/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * Per-app public-URL auth write shape (issue #477 / ADR-077). Sent on PATCH /v1/apps/{slug}; apid seals the basic_user + basic_pass into a single APP_BASIC_AUTH secretbox blob before persistence. The plaintext is never echoed on read (see PublicAuthStatus).
 */
export type PublicAuthBlock = {
  /**
   * Auth mode (closed set). 'open' is the pre-#477 default (every request passes). 'bearer' requires Authorization: Bearer (Hobby+; 402 on Free). 'basic' requires HTTP Basic auth with sealed credentials (Pro+; 402 on Free/Hobby). Unknown values → 422 invalid_public_auth_mode.
   */
  mode: 'open' | 'bearer' | 'basic';
  /**
   * Basic-auth username (RFC 7617 §2). Plaintext at PATCH time; sealed into apps.public_auth_basic under the APP_BASIC_AUTH secretbox namespace. Required when mode='basic'; ignored otherwise. Range [1, 128] bytes after TrimSpace.
   */
  basic_user?: string;
  /**
   * Basic-auth password (RFC 7617 §2). Plaintext at PATCH time; sealed alongside basic_user under the APP_BASIC_AUTH secretbox namespace. Required when mode='basic'; ignored otherwise. Range [1, 256] bytes.
   */
  basic_pass?: string;
};


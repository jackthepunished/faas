/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * Read-only per-app public-URL auth shape on AppResponse (issue #477 / ADR-077). Mirrors the row contents without the plaintext credentials. The redaction posture is a load-bearing invariant — see ADR-077 §Decision 're-redaction invariant': neither basic_user nor basic_pass is EVER returned on the wire, even when mode='basic'. To rotate credentials, the customer PATCHes a fresh public_auth block.
 */
export type PublicAuthStatus = {
  /**
   * Active auth mode. One of 'open', 'bearer', 'basic'. Matches apps.public_auth_mode on disk; a PATCH 'open' cleared any prior sealed blob so a stale secretbox row never reaches a fresh request.
   */
  mode: 'open' | 'bearer' | 'basic';
  /**
   * True iff the row carries a non-null apps.public_auth_basic blob (i.e. mode='basic' with credentials). A mode='basic' row without creds would 401 every request — has_basic_creds is the operator-greppable signal that the seal succeeded.
   */
  has_basic_creds: boolean;
};


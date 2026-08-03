/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * POST /v1/orgs/{slug}/keys body. Mirrors `CreateKeyRequest` (PR 6 keeps them in lockstep) — label + optional scopes. Empty `scopes` defaults to `["admin"]` so existing callers preserve the legacy full-access behavior. See IAM-1, ADR-034 rev2.
 */
export type CreateOrgAPIKeyRequest = {
  label?: string;
  /**
   * Requested permission set for the org-scoped key. Server validates each entry against the closed vocabulary and rejects unknown scopes at mint time. `admin` is the legacy full-access scope; the other five cover narrower surfaces (see APIKeyResponse.scopes). See IAM-1, ADR-034 rev2. PR 6 keeps the legacy and org-scoped shapes in lockstep so SDK callers can swap one request body for the other.
   */
  scopes?: Array<'admin' | 'deploy:write' | 'secrets:read' | 'secrets:write' | 'usage:read' | 'apps:read' | 'env:read' | 'env:write'>;
};


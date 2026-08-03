/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * API key metadata: id, prefix (first 8 chars), label, scopes, created/last-used timestamps, request count. **Plaintext is returned only on POST**.
 */
export type APIKeyResponse = {
  id: string;
  /**
   * First 16 chars of the key (e.g. `fp_live_abc12345…`).
   */
  prefix: string;
  label?: string | null;
  /**
   * Permission set attached to the key. Closed vocabulary (IAM-1, ADR-034 rev2): admin is the legacy full-access scope; apps:read covers GETs across the apps/deployments/audit/secrets-list surface; deploy:write covers POST/PUT/PATCH/DELETE on apps+queues; secrets:write covers PUT/DELETE on /apps/{slug}/secrets/{key}; usage:read covers GET /v1/usage*.
   */
  scopes: Array<'admin' | 'deploy:write' | 'secrets:read' | 'secrets:write' | 'usage:read' | 'apps:read' | 'env:read' | 'env:write'>;
  last_used_at?: string | null;
  created_at: string;
  /**
   * When the key expires (RFC 3339). Absent on never-expiring admin keys.
   */
  expires_at?: string | null;
  /**
   * Status state machine. `active` = ready; `grace` = in the post-rotation window; `revoked` = terminal.
   */
  status?: 'active' | 'grace' | 'revoked';
  /**
   * When the key was revoked (RFC 3339). Absent on active/grace keys.
   */
  revoked_at?: string | null;
  /**
   * Predecessor key id when this row was minted by rotateKey. Absent on a fresh mint.
   */
  rotated_from_id?: string | null;
  /**
   * PRESENT ONLY on POST /v1/keys and POST /v1/keys/{id}/rotate responses. Never returned again.
   */
  plaintext?: string | null;
};


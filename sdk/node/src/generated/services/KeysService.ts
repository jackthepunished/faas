/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { APIKeyResponse } from '../models/APIKeyResponse.js';
import type { CreateKeyRequest } from '../models/CreateKeyRequest.js';
import type { GraceWindowResponse } from '../models/GraceWindowResponse.js';
import type { RotateKeyResponse } from '../models/RotateKeyResponse.js';
import type { SetGraceWindowRequest } from '../models/SetGraceWindowRequest.js';
import type { CancelablePromise } from '../core/CancelablePromise.js';
import { OpenAPI } from '../core/OpenAPI.js';
import { request as __request } from '../core/request.js';
export class KeysService {
  /**
   * List API keys (no plaintext).
   * @returns APIKeyResponse API key metadata on the account. Plaintext is never returned.
   * @throws ApiError
   */
  public static listKeys(): CancelablePromise<Array<APIKeyResponse>> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/v1/keys',
      errors: {
        401: `code: unauthorized`,
        429: `429. Two response shapes:
        - \`application/problem+json\` for code-driven 429s (\`plan_limit_concurrency\`, \`quota_exhausted\`).
        - \`text/plain\` for the authlimiter middleware (\`pkg/middleware/authlimit.go\`).
        `,
      },
    });
  }
  /**
   * Mint a new API key.
   * Returns the plaintext key **once**. The plaintext is never stored
   * and cannot be retrieved; subsequent GETs return only the prefix.
   *
   * @returns APIKeyResponse The new API key. **The plaintext is returned exactly once** — every subsequent GET returns only the prefix.
   * @throws ApiError
   */
  public static createKey({
    requestBody,
  }: {
    /**
     * Create a new API key for the authenticated account. Plaintext is returned exactly once in the 201 response and cannot be recovered later — store it immediately. See IAM-1, ADR-034 rev2 for the scope vocabulary.
     */
    requestBody: CreateKeyRequest,
  }): CancelablePromise<APIKeyResponse> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/v1/keys',
      body: requestBody,
      mediaType: 'application/json',
      errors: {
        401: `code: unauthorized`,
        429: `429. Two response shapes:
        - \`application/problem+json\` for code-driven 429s (\`plan_limit_concurrency\`, \`quota_exhausted\`).
        - \`text/plain\` for the authlimiter middleware (\`pkg/middleware/authlimit.go\`).
        `,
      },
    });
  }
  /**
   * Revoke an API key.
   * @returns void
   * @throws ApiError
   */
  public static deleteKey({
    id,
  }: {
    /**
     * 32-hex-char opaque ID (NOT canonical UUID).
     */
    id: string,
  }): CancelablePromise<void> {
    return __request(OpenAPI, {
      method: 'DELETE',
      url: '/v1/keys/{id}',
      path: {
        'id': id,
      },
      errors: {
        401: `code: unauthorized`,
        404: `code: not_found`,
        429: `429. Two response shapes:
        - \`application/problem+json\` for code-driven 429s (\`plan_limit_concurrency\`, \`quota_exhausted\`).
        - \`text/plain\` for the authlimiter middleware (\`pkg/middleware/authlimit.go\`).
        `,
      },
    });
  }
  /**
   * Rotate an API key.
   * Mints a new key (status='active') and demotes the old key in
   * a single transaction (issue #189 / IAM-5). The new key
   * inherits the predecessor's label + scopes so the customer's
   * CI config does not need to chase a label change.
   *
   * The old key's `expires_at` is OVERWRITTEN to the grace
   * deadline (now + grace_window_days, where grace_window_days
   * resolves from the per-account override or the 7-day plan
   * default). Status flips to 'grace' for the window, then to
   * 'revoked' at the deadline. Setting
   * `accounts.key_grace_window_days = 0` makes rotation atomic
   * (old key revoked immediately).
   *
   * Returns the new plaintext exactly once — the old plaintext
   * is not re-issued (we only store the SHA-256 hash). The
   * customer's CI script captures the new plaintext at the
   * moment of rotation and uses the new key thereafter; the old
   * key remains valid only for the grace window.
   *
   * Quota: rotation is quota-neutral (-1 +1 = 0 net). A
   * customer AT the per-account cap can still rotate.
   *
   * @returns RotateKeyResponse New key minted, old key now in 'grace' (or 'revoked' if grace_window_days=0).
   * @throws ApiError
   */
  public static rotateKey({
    id,
  }: {
    /**
     * 32-hex-char opaque ID (NOT canonical UUID).
     */
    id: string,
  }): CancelablePromise<RotateKeyResponse> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/v1/keys/{id}/rotate',
      path: {
        'id': id,
      },
      errors: {
        401: `code: unauthorized`,
        404: `No such key, or the key is already revoked.`,
        429: `429. Two response shapes:
        - \`application/problem+json\` for code-driven 429s (\`plan_limit_concurrency\`, \`quota_exhausted\`).
        - \`text/plain\` for the authlimiter middleware (\`pkg/middleware/authlimit.go\`).
        `,
      },
    });
  }
  /**
   * Read the per-account rotation grace-window override.
   * Returns the customer's current override (the value stored in
   * `accounts.key_grace_window_days`) and the plan default
   * alongside. `days=null` means "no override" — the rotation
   * handler uses the plan default (api.DefaultAPIKeyGraceWindowDays = 7).
   *
   * @returns GraceWindowResponse Current override + plan default.
   * @throws ApiError
   */
  public static getGraceWindow(): CancelablePromise<GraceWindowResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/v1/account/keys/grace_window_days',
      errors: {
        401: `code: unauthorized`,
        429: `429. Two response shapes:
        - \`application/problem+json\` for code-driven 429s (\`plan_limit_concurrency\`, \`quota_exhausted\`).
        - \`text/plain\` for the authlimiter middleware (\`pkg/middleware/authlimit.go\`).
        `,
      },
    });
  }
  /**
   * Set the per-account rotation grace-window override.
   * Writes the override and invalidates the in-process cache so
   * the next rotation observes the new value. `days=0` is atomic
   * rotation (no grace); `days=null` (or omission) clears the
   * override and falls back to the plan default.
   *
   * @returns GraceWindowResponse Override applied.
   * @throws ApiError
   */
  public static setGraceWindow({
    requestBody,
  }: {
    requestBody: SetGraceWindowRequest,
  }): CancelablePromise<GraceWindowResponse> {
    return __request(OpenAPI, {
      method: 'PATCH',
      url: '/v1/account/keys/grace_window_days',
      body: requestBody,
      mediaType: 'application/json',
      errors: {
        400: `Invalid body (e.g. negative days).`,
        401: `code: unauthorized`,
        429: `429. Two response shapes:
        - \`application/problem+json\` for code-driven 429s (\`plan_limit_concurrency\`, \`quota_exhausted\`).
        - \`text/plain\` for the authlimiter middleware (\`pkg/middleware/authlimit.go\`).
        `,
      },
    });
  }
}

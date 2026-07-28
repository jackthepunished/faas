/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { AppEnvListResponse } from '../models/AppEnvListResponse.js';
import type { AppEnvResponse } from '../models/AppEnvResponse.js';
import type { PutAppEnvRequest } from '../models/PutAppEnvRequest.js';
import type { CancelablePromise } from '../core/CancelablePromise.js';
import { OpenAPI } from '../core/OpenAPI.js';
import { request as __request } from '../core/request.js';
export class EnvService {
  /**
   * List env vars on an app.
   * Returns every env var key + timestamps on the app. The plaintext
   * value NEVER appears in the response — guest-init reads the value
   * at process start from `/etc/faas/env.json` inside the guest.
   *
   * @returns AppEnvListResponse Env var envelopes on the app (plaintext never returned).
   * @throws ApiError
   */
  public static listEnv({
    slug,
  }: {
    /**
     * App slug. Lowercase letters, digits, hyphens; must start and end with alnum.
     */
    slug: string,
  }): CancelablePromise<AppEnvListResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/v1/apps/{slug}/env',
      path: {
        'slug': slug,
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
   * Set an env var.
   * Persists the plaintext value verbatim in the app_envs table (no
   * seal step). Env vars are non-sensitive runtime config by contract
   * — credentials stay on `/v1/apps/{slug}/secrets/{key}`. Applies on
   * next wake (cold-boot OR snapshot-restore); the running instance
   * is unaffected.
   *
   * @returns AppEnvResponse The stored env var envelope.
   * @throws ApiError
   */
  public static setEnv({
    slug,
    key,
    requestBody,
  }: {
    /**
     * App slug. Lowercase letters, digits, hyphens; must start and end with alnum.
     */
    slug: string,
    /**
     * Secret key. Must start with a letter; A-Z, 0-9, underscore.
     */
    key: string,
    /**
     * Env var payload — key name + plaintext.
     */
    requestBody: PutAppEnvRequest,
  }): CancelablePromise<AppEnvResponse> {
    return __request(OpenAPI, {
      method: 'PUT',
      url: '/v1/apps/{slug}/env/{key}',
      path: {
        'slug': slug,
        'key': key,
      },
      body: requestBody,
      mediaType: 'application/json',
      errors: {
        400: `code: env_var_invalid_key`,
        401: `code: unauthorized`,
        403: `code: plan_limit_env_vars`,
        413: `code: env_value_too_large`,
        429: `429. Two response shapes:
        - \`application/problem+json\` for code-driven 429s (\`plan_limit_concurrency\`, \`quota_exhausted\`).
        - \`text/plain\` for the authlimiter middleware (\`pkg/middleware/authlimit.go\`).
        `,
      },
    });
  }
  /**
   * Delete an env var.
   * @returns void
   * @throws ApiError
   */
  public static deleteEnv({
    slug,
    key,
  }: {
    /**
     * App slug. Lowercase letters, digits, hyphens; must start and end with alnum.
     */
    slug: string,
    /**
     * Secret key. Must start with a letter; A-Z, 0-9, underscore.
     */
    key: string,
  }): CancelablePromise<void> {
    return __request(OpenAPI, {
      method: 'DELETE',
      url: '/v1/apps/{slug}/env/{key}',
      path: {
        'slug': slug,
        'key': key,
      },
      errors: {
        400: `code: env_var_not_found`,
        401: `code: unauthorized`,
        404: `code: not_found`,
        429: `429. Two response shapes:
        - \`application/problem+json\` for code-driven 429s (\`plan_limit_concurrency\`, \`quota_exhausted\`).
        - \`text/plain\` for the authlimiter middleware (\`pkg/middleware/authlimit.go\`).
        `,
      },
    });
  }
}

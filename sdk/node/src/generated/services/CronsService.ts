/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { CreateCronRequest } from '../models/CreateCronRequest.js';
import type { CronResponse } from '../models/CronResponse.js';
import type { ListCronRunsResponse } from '../models/ListCronRunsResponse.js';
import type { UpdateCronRequest } from '../models/UpdateCronRequest.js';
import type { CancelablePromise } from '../core/CancelablePromise.js';
import { OpenAPI } from '../core/OpenAPI.js';
import { request as __request } from '../core/request.js';
export class CronsService {
  /**
   * List cron triggers.
   * @returns CronResponse Cron triggers on the account.
   * @throws ApiError
   */
  public static listCrons(): CancelablePromise<Array<CronResponse>> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/v1/crons',
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
   * Create a cron trigger.
   * @returns CronResponse The new cron trigger.
   * @throws ApiError
   */
  public static createCron({
    requestBody,
    idempotencyKey,
  }: {
    /**
     * Cron payload — schedule expression + target URL. See CreateCronRequest.
     */
    requestBody: CreateCronRequest,
    /**
     * Idempotency key for the POST. Stored for 24h. On replay the server
     * returns the original response with `Idempotent-Replayed: true`.
     *
     */
    idempotencyKey?: string,
  }): CancelablePromise<CronResponse> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/v1/crons',
      headers: {
        'Idempotency-Key': idempotencyKey,
      },
      body: requestBody,
      mediaType: 'application/json',
      errors: {
        400: `code: cron_invalid | plan_crons_not_allowed | plan_cron_quota`,
        401: `code: unauthorized`,
        402: `code: cron_invalid | plan_crons_not_allowed | plan_cron_quota`,
        403: `code: plan_limit_apps | plan_limit_ram | plan_limit_concurrency | plan_min_instances_not_allowed | plan_limit_secrets | plan_cron_quota | app_layer_too_large | image_egress_denied`,
        429: `429. Two response shapes:
        - \`application/problem+json\` for code-driven 429s (\`plan_limit_concurrency\`, \`quota_exhausted\`).
        - \`text/plain\` for the authlimiter middleware (\`pkg/middleware/authlimit.go\`).
        `,
      },
    });
  }
  /**
   * Partial-update a cron.
   * @returns CronResponse The updated cron trigger.
   * @throws ApiError
   */
  public static updateCron({
    id,
    requestBody,
  }: {
    /**
     * 32-hex-char opaque ID (NOT canonical UUID).
     */
    id: string,
    /**
     * Cron patch — every field is optional. See UpdateCronRequest.
     */
    requestBody: UpdateCronRequest,
  }): CancelablePromise<CronResponse> {
    return __request(OpenAPI, {
      method: 'PATCH',
      url: '/v1/crons/{id}',
      path: {
        'id': id,
      },
      body: requestBody,
      mediaType: 'application/json',
      errors: {
        400: `code: cron_invalid | plan_crons_not_allowed | plan_cron_quota`,
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
   * Delete a cron.
   * @returns void
   * @throws ApiError
   */
  public static deleteCron({
    id,
  }: {
    /**
     * 32-hex-char opaque ID (NOT canonical UUID).
     */
    id: string,
  }): CancelablePromise<void> {
    return __request(OpenAPI, {
      method: 'DELETE',
      url: '/v1/crons/{id}',
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
   * List recent runs of a cron.
   * Execution history for one cron, newest first. Each row reports
   * when the fire started, how long it took (`duration_ms`,
   * computed server-side), and a normalized `outcome` — so a
   * timeout is distinguishable from a generic failure without
   * parsing `error`.
   *
   * Paginated by `?before=<id>` (the LAST id of the returned
   * slice). Defaults to 10 per page; capped at 100. For a wider,
   * cross-source view use `GET /v1/invocations`.
   *
   * @returns ListCronRunsResponse A page of runs for this cron, newest first.
   * @throws ApiError
   */
  public static listCronRuns({
    id,
    before,
    limit = 10,
  }: {
    /**
     * 32-hex-char opaque ID (NOT canonical UUID).
     */
    id: string,
    /**
     * Cursor — return runs older than this run id. Omit for the most recent page.
     */
    before?: string,
    /**
     * Page size; 1-100, default 10.
     */
    limit?: number,
  }): CancelablePromise<ListCronRunsResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/v1/crons/{id}/runs',
      path: {
        'id': id,
      },
      query: {
        'before': before,
        'limit': limit,
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
}

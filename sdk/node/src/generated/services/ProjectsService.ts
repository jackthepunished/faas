/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { ApplyResponse } from '../models/ApplyResponse.js';
import type { PlanResponse } from '../models/PlanResponse.js';
import type { ProjectApplyRequest } from '../models/ProjectApplyRequest.js';
import type { ProjectScanRequest } from '../models/ProjectScanRequest.js';
import type { CancelablePromise } from '../core/CancelablePromise.js';
import { OpenAPI } from '../core/OpenAPI.js';
import { request as __request } from '../core/request.js';
export class ProjectsService {
  /**
   * Scan an uploaded tarball and return a deploy plan.
   * Dry-run. Accepts a multipart upload (`source=<tar.gz>`,
   * `project_slug`, `production_branch`, `install_id`, `only`)
   * and returns a PlanResponse with the discovered workloads,
   * managed services, derived scan_source, and a plan_token
   * that the apply endpoint can echo back to skip the
   * second extract.
   *
   * On over-quota the response carries `can_apply=false`
   * (and `crons_not_allowed=true` for Free plan) so the CLI
   * can branch without a second request.
   *
   * @returns PlanResponse The deploy plan.
   * @throws ApiError
   */
  public static scanProject({
    formData,
  }: {
    formData: ProjectScanRequest,
  }): CancelablePromise<PlanResponse> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/v1/projects/scan',
      formData: formData,
      mediaType: 'multipart/form-data',
      errors: {
        400: `code: validation_failed | source_invalid | build_undetected | handler_missing | image_required | cron_invalid | secret_invalid_key`,
        401: `code: unauthorized`,
        402: `code: cron_invalid | plan_crons_not_allowed | plan_cron_quota`,
        403: `code: plan_limit_apps | plan_limit_ram | plan_limit_concurrency | plan_min_instances_not_allowed | plan_limit_secrets | plan_cron_quota | app_layer_too_large | image_egress_denied`,
        413: `code: source_too_large`,
        429: `429. Two response shapes:
        - \`application/problem+json\` for code-driven 429s (\`plan_limit_concurrency\`, \`quota_exhausted\`).
        - \`text/plain\` for the authlimiter middleware (\`pkg/middleware/authlimit.go\`).
        `,
      },
    });
  }
  /**
   * Apply a deploy plan in one transaction.
   * Accepts the same multipart body as /scan plus an optional
   * `plan_token` query parameter echoing the dry-run token. On
   * success the response carries the inserted project_id and
   * per-app IDs so the CLI's `--yes` flow can render
   * `applied: <slug> → <app_id>`. On quota the response is
   * the matching RFC 7807 problem (402 Free crons, 403 apps
   * or cron cap) with zero rows inserted.
   *
   * The apply handler resolves workload-name → app_id from
   * the just-inserted apps and inserts crons in a follow-up
   * pass; the quota check ran inside ApplyProjectPlan's Tx.
   *
   * @returns ApplyResponse The applied project + apps + crons.
   * @throws ApiError
   */
  public static applyProject({
    formData,
    planToken,
    idempotencyKey,
  }: {
    formData: ProjectApplyRequest,
    /**
     * Echo of the dry-run plan_token (base64-JSON). Omit to skip the cache.
     */
    planToken?: string,
    /**
     * Idempotency key for the POST. Stored for 24h. On replay the server
     * returns the original response with `Idempotent-Replayed: true`.
     *
     */
    idempotencyKey?: string,
  }): CancelablePromise<ApplyResponse> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/v1/projects',
      headers: {
        'Idempotency-Key': idempotencyKey,
      },
      query: {
        'plan_token': planToken,
      },
      formData: formData,
      mediaType: 'multipart/form-data',
      errors: {
        400: `code: validation_failed | source_invalid | build_undetected | handler_missing | image_required | cron_invalid | secret_invalid_key`,
        401: `code: unauthorized`,
        402: `code: cron_invalid | plan_crons_not_allowed | plan_cron_quota`,
        403: `code: plan_limit_apps | plan_limit_ram | plan_limit_concurrency | plan_min_instances_not_allowed | plan_limit_secrets | plan_cron_quota | app_layer_too_large | image_egress_denied`,
        409: `code: validation_failed | source_invalid | build_undetected | handler_missing | image_required | cron_invalid | secret_invalid_key`,
        413: `code: source_too_large`,
        429: `429. Two response shapes:
        - \`application/problem+json\` for code-driven 429s (\`plan_limit_concurrency\`, \`quota_exhausted\`).
        - \`text/plain\` for the authlimiter middleware (\`pkg/middleware/authlimit.go\`).
        `,
      },
    });
  }
}

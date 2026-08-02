/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { AppMetricsResponse } from '../models/AppMetricsResponse.js';
import type { AppResponse } from '../models/AppResponse.js';
import type { AppsMetricsResponse } from '../models/AppsMetricsResponse.js';
import type { CreateAppRequest } from '../models/CreateAppRequest.js';
import type { RenameAppRequest } from '../models/RenameAppRequest.js';
import type { UpdateAppRequest } from '../models/UpdateAppRequest.js';
import type { WakeTimelineResponse } from '../models/WakeTimelineResponse.js';
import type { CancelablePromise } from '../core/CancelablePromise.js';
import { OpenAPI } from '../core/OpenAPI.js';
import { request as __request } from '../core/request.js';
export class AppsService {
  /**
   * List apps on the account.
   * @returns AppResponse Apps on the account.
   * @throws ApiError
   */
  public static listApps(): CancelablePromise<Array<AppResponse>> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/v1/apps',
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
   * Create an app.
   * @returns AppResponse The new app.
   * @throws ApiError
   */
  public static createApp({
    requestBody,
    idempotencyKey,
  }: {
    /**
     * App creation payload (slug, type, runtime, RAM, …). See CreateAppRequest schema.
     */
    requestBody: CreateAppRequest,
    /**
     * Idempotency key for the POST. Stored for 24h. On replay the server
     * returns the original response with `Idempotent-Replayed: true`.
     *
     */
    idempotencyKey?: string,
  }): CancelablePromise<AppResponse> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/v1/apps',
      headers: {
        'Idempotency-Key': idempotencyKey,
      },
      body: requestBody,
      mediaType: 'application/json',
      errors: {
        400: `code: validation_failed | source_invalid | build_undetected | handler_missing | image_required | cron_invalid | secret_invalid_key`,
        401: `code: unauthorized`,
        403: `code: plan_limit_apps | plan_limit_ram | plan_limit_concurrency | plan_min_instances_not_allowed | plan_limit_secrets | plan_cron_quota | app_layer_too_large | image_egress_denied`,
        409: `code: conflict`,
        429: `429. Two response shapes:
        - \`application/problem+json\` for code-driven 429s (\`plan_limit_concurrency\`, \`quota_exhausted\`).
        - \`text/plain\` for the authlimiter middleware (\`pkg/middleware/authlimit.go\`).
        `,
      },
    });
  }
  /**
   * Fetch one app.
   * @returns AppResponse The app.
   * @throws ApiError
   */
  public static getApp({
    slug,
  }: {
    /**
     * App slug. Lowercase letters, digits, hyphens; must start and end with alnum.
     */
    slug: string,
  }): CancelablePromise<AppResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/v1/apps/{slug}',
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
   * Partial-update an app.
   * @returns AppResponse The updated app.
   * @throws ApiError
   */
  public static updateApp({
    slug,
    requestBody,
  }: {
    /**
     * App slug. Lowercase letters, digits, hyphens; must start and end with alnum.
     */
    slug: string,
    /**
     * Patch payload — every field is optional; omitted fields are unchanged. See UpdateAppRequest.
     */
    requestBody: UpdateAppRequest,
  }): CancelablePromise<AppResponse> {
    return __request(OpenAPI, {
      method: 'PATCH',
      url: '/v1/apps/{slug}',
      path: {
        'slug': slug,
      },
      body: requestBody,
      mediaType: 'application/json',
      errors: {
        400: `code: validation_failed | source_invalid | build_undetected | handler_missing | image_required | cron_invalid | secret_invalid_key`,
        401: `code: unauthorized`,
        403: `code: plan_limit_apps | plan_limit_ram | plan_limit_concurrency | plan_min_instances_not_allowed | plan_limit_secrets | plan_cron_quota | app_layer_too_large | image_egress_denied`,
        404: `code: not_found`,
        422: `code: invalid_min_instances — must be in [0, plan max_concurrency].`,
        429: `429. Two response shapes:
        - \`application/problem+json\` for code-driven 429s (\`plan_limit_concurrency\`, \`quota_exhausted\`).
        - \`text/plain\` for the authlimiter middleware (\`pkg/middleware/authlimit.go\`).
        `,
      },
    });
  }
  /**
   * Delete an app.
   * @returns void
   * @throws ApiError
   */
  public static deleteApp({
    slug,
  }: {
    /**
     * App slug. Lowercase letters, digits, hyphens; must start and end with alnum.
     */
    slug: string,
  }): CancelablePromise<void> {
    return __request(OpenAPI, {
      method: 'DELETE',
      url: '/v1/apps/{slug}',
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
   * Per-app request metrics (issue
   * Time-windowed rollup of one app's gateway activity. The `range`
   * parameter is a closed vocabulary bounded by Prometheus
   * retention (`prom_retention_days: 15`):
   *
   * `5m` (default) | `15m` | `1h` | `6h` | `24h` | `7d` | `15d`
   *
   * Wake latency (`wake_p95_ms`) is the FLEET p95
   * (`gateway_wake_latency_seconds` is unlabeled). On Prometheus
   * failure the endpoint returns 200 with zeroed fields and
   * `source: "degraded: <reason>"`, matching the public status
   * page contract.
   *
   * @returns AppMetricsResponse The metrics snapshot.
   * @throws ApiError
   */
  public static getAppMetrics({
    slug,
    range = '5m',
  }: {
    /**
     * App slug. Lowercase letters, digits, hyphens; must start and end with alnum.
     */
    slug: string,
    /**
     * Time window. Default `5m`.
     */
    range?: '5m' | '15m' | '1h' | '6h' | '24h' | '7d' | '15d',
  }): CancelablePromise<AppMetricsResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/v1/apps/{slug}/metrics',
      path: {
        'slug': slug,
      },
      query: {
        'range': range,
      },
      errors: {
        400: `code: validation_failed | source_invalid | build_undetected | handler_missing | image_required | cron_invalid | secret_invalid_key`,
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
   * Manually park all running instances.
   * @returns void
   * @throws ApiError
   */
  public static parkApp({
    slug,
  }: {
    /**
     * App slug. Lowercase letters, digits, hyphens; must start and end with alnum.
     */
    slug: string,
  }): CancelablePromise<void> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/v1/apps/{slug}/park',
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
   * Manually wake an instance.
   * @returns void
   * @throws ApiError
   */
  public static wakeApp({
    slug,
  }: {
    /**
     * App slug. Lowercase letters, digits, hyphens; must start and end with alnum.
     */
    slug: string,
  }): CancelablePromise<void> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/v1/apps/{slug}/wake',
      path: {
        'slug': slug,
      },
      errors: {
        401: `code: unauthorized`,
        404: `code: not_found`,
        429: `code: plan_limit_concurrency`,
        503: `code: capacity_unavailable — no host headroom (alerting; should be near-impossible).`,
      },
    });
  }
  /**
   * Rename an app.
   * @returns AppResponse The renamed app.
   * @throws ApiError
   */
  public static renameApp({
    slug,
    requestBody,
    idempotencyKey,
  }: {
    /**
     * App slug. Lowercase letters, digits, hyphens; must start and end with alnum.
     */
    slug: string,
    /**
     * Rename payload — the new slug. See RenameAppRequest.
     */
    requestBody: RenameAppRequest,
    /**
     * Idempotency key for the POST. Stored for 24h. On replay the server
     * returns the original response with `Idempotent-Replayed: true`.
     *
     */
    idempotencyKey?: string,
  }): CancelablePromise<AppResponse> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/v1/apps/{slug}/rename',
      path: {
        'slug': slug,
      },
      headers: {
        'Idempotency-Key': idempotencyKey,
      },
      body: requestBody,
      mediaType: 'application/json',
      errors: {
        400: `code: validation_failed | source_invalid | build_undetected | handler_missing | image_required | cron_invalid | secret_invalid_key`,
        401: `code: unauthorized`,
        409: `code: app_rename_failed — slug taken by another live app, or DB unique violation.`,
      },
    });
  }
  /**
   * Stream app logs (SSE).
   * Server-Sent Events stream of instance logs. NOTE: this endpoint is
   * currently mounted behind `s.authLimited` and is documented here for
   * reference; the dashboard and CLI also consume it directly.
   *
   * @returns any A text/event-stream of structured log lines, terminated by an empty SSE frame when the connection closes.
   * @throws ApiError
   */
  public static streamAppLogs({
    slug,
    follow = 0,
    grep,
    since,
    level,
  }: {
    /**
     * App slug. Lowercase letters, digits, hyphens; must start and end with alnum.
     */
    slug: string,
    /**
     * If 1, hold the connection open and stream new entries.
     */
    follow?: 0 | 1,
    /**
     * Substring filter applied to each log line.
     */
    grep?: string,
    /**
     * RFC3339 lower-bound on the line timestamp.
     */
    since?: string,
    /**
     * Exact match on the structured `level` field (info, warn, or error). Empty = no level filter. The CLI and the apid handler both validate against the same enum (api.IsValidLogLevel in pkg/api/logs.go); an unknown value short-circuits with an SSE error frame carrying code invalid_level.
     *
     */
    level?: 'info' | 'warn' | 'error',
  }): CancelablePromise<any> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/v1/apps/{slug}/logs',
      path: {
        'slug': slug,
      },
      query: {
        'follow': follow,
        'grep': grep,
        'since': since,
        'level': level,
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
   * Account-wide per-app metrics rollup.
   * One call replaces N per-app `/v1/apps/{slug}/metrics` calls
   * (issue #393). Returns the same `AppMetricsResponse` shape
   * per app, keyed by `app_slug`, so the dashboard can render
   * all apps on a single page without a per-app fan-out.
   *
   * Range is the closed vocabulary from the per-app endpoint:
   * `5m` (default) | `15m` | `1h` | `6h` | `24h` | `7d` | `15d`.
   * Prometheus failure short-circuits the entire response
   * (never partial-populated) and emits `source:
   * "degraded: <reason>"` with zeroed `apps`, matching the
   * per-app contract exactly.
   *
   * PromQL cost: 6 round-trips regardless of N apps (vs. 7N
   * for the naive per-app loop) — see `pkg/promql.Client.QueryMap`
   * and `Client.QueryBuckets`.
   *
   * @returns AppsMetricsResponse The rollup.
   * @throws ApiError
   */
  public static getAppsMetrics({
    range = '5m',
  }: {
    /**
     * Time window applied to every per-app rollup row. Default `5m`.
     */
    range?: '5m' | '15m' | '1h' | '6h' | '24h' | '7d' | '15d',
  }): CancelablePromise<AppsMetricsResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/v1/apps/metrics',
      query: {
        'range': range,
      },
      errors: {
        400: `code: validation_failed | source_invalid | build_undetected | handler_missing | image_required | cron_invalid | secret_invalid_key`,
        401: `code: unauthorized`,
        429: `429. Two response shapes:
        - \`application/problem+json\` for code-driven 429s (\`plan_limit_concurrency\`, \`quota_exhausted\`).
        - \`text/plain\` for the authlimiter middleware (\`pkg/middleware/authlimit.go\`).
        `,
      },
    });
  }
  /**
   * List the canonical wake-timeline frames for one wake.
   * Oldest-first (forward narrative). Returns every typed
   * `wake.*` events row for the given wake_id: queue_accepted
   * → admitted → boot_started → boot_completed →
   * readiness_200 → proxy_first_byte. Build and deploy
   * failures (`wake.build_failed`, `wake.deploy_failed`,
   * `wake.boot_failed`) are joined in alongside the success
   * path so a single GET shows the whole lifecycle.
   *
   * The endpoint is a sub-resource of `/v1/apps/{slug}`;
   * auth and rate-limit share the §12 per-app budget with
   * logs/metrics/wake. Cross-account access 404s the
   * same way unknown slugs do (forge-proof: every row's
   * `data.app_id` is verified to match the resolved app).
   *
   * @returns WakeTimelineResponse Wake-timeline frames.
   * @throws ApiError
   */
  public static listWakeTimeline({
    slug,
    wakeId,
    since,
    limit = 200,
  }: {
    /**
     * App slug (lowercase, kebab-case; per-account unique).
     */
    slug: string,
    /**
     * The per-wake correlation handle minted by the schedd
     * engine (UUID v4 in production). The endpoint returns
     * every `wake.*` events row whose `data.wake_id`
     * matches — the partial index `events_wake_id_idx`
     * (migrations/00107) serves the read in O(frames)
     * regardless of the events table size.
     *
     */
    wakeId: string,
    /**
     * Only return rows with `at >= since` (RFC 3339).
     */
    since?: string,
    /**
     * Max frames to return. Silently capped at 1000.
     */
    limit?: number,
  }): CancelablePromise<WakeTimelineResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/v1/apps/{slug}/wakes/{wake_id}/timeline',
      path: {
        'slug': slug,
        'wake_id': wakeId,
      },
      query: {
        'since': since,
        'limit': limit,
      },
      errors: {
        400: `Malformed query parameter on the wake-timeline read — \`since\` not RFC 3339 or \`limit\` out of range.`,
        401: `code: unauthorized`,
        404: `No such app (slug) or wake_id is unknown.`,
        429: `429. Two response shapes:
        - \`application/problem+json\` for code-driven 429s (\`plan_limit_concurrency\`, \`quota_exhausted\`).
        - \`text/plain\` for the authlimiter middleware (\`pkg/middleware/authlimit.go\`).
        `,
      },
    });
  }
}

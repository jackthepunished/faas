/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { Trace } from '../models/Trace.js';
import type { WakeTimelineResponse } from '../models/WakeTimelineResponse.js';
import type { CancelablePromise } from '../core/CancelablePromise.js';
import { OpenAPI } from '../core/OpenAPI.js';
import { request as __request } from '../core/request.js';
export class ObservabilityService {
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
     * (migrations/00113) serves the read in O(frames)
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
  /**
   * Retrieve a stored OpenTelemetry trace.
   * Issue #555. Returns the full span tree for a single trace_id
   * (32-hex, the same as the request's `wake_id`). The trace is
   * sourced from the gatewayd-public in-memory ring (24h
   * retention, 100k default cap). When no OTLP endpoint is set
   * the ring is the only source — when OTLP is set, the ring
   * still operates as the customer-facing query layer.
   *
   * Authentication: the `X-Faas-Trace-Auth` header must carry
   * the operator's observer token (env: `FAAS_TRACE_OBSERVER_TOKEN`).
   * The endpoint is gated even when the dashboard session cookie is
   * present — tracing is an operator surface, not a customer one.
   * An empty token disables the endpoint (returns 404).
   *
   * @returns Trace The trace tree.
   * @throws ApiError
   */
  public static getTrace({
    traceId,
  }: {
    /**
     * 32-hex W3C trace_id (or wake_id UUIDv7 hex form).
     */
    traceId: string,
  }): CancelablePromise<Trace> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/v1/traces/{trace_id}',
      path: {
        'trace_id': traceId,
      },
      errors: {
        401: `Missing or wrong observer token. The endpoint is operator-
        gated; the customer-facing dashboard session does not
        grant access.
        `,
        404: `Trace not in the ring (never seen, or evicted by the 24h
        retention sweep / LRU cap).
        `,
      },
    });
  }
}

/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { BuildProvenanceResponse } from '../models/BuildProvenanceResponse.js';
import type { CreateDeploymentRequest } from '../models/CreateDeploymentRequest.js';
import type { DeploymentListResponse } from '../models/DeploymentListResponse.js';
import type { DeploymentResponse } from '../models/DeploymentResponse.js';
import type { CancelablePromise } from '../core/CancelablePromise.js';
import { OpenAPI } from '../core/OpenAPI.js';
import { request as __request } from '../core/request.js';
export class DeploymentsService {
  /**
   * Create a deployment.
   * Two content-types are accepted:
   * - `application/json` (`CreateDeploymentRequest` with an `image` field): prebuilt OCI reference.
   * - `multipart/form-data`: source tarball upload (or Dockerfile escape hatch).
   * Source size is plan-capped (Free/Hobby 100 MB, Pro/Scale 250 MB).
   *
   * @returns DeploymentResponse The deployment whose build has been accepted and queued.
   * @throws ApiError
   */
  public static createDeployment({
    slug,
    requestBody,
    idempotencyKey,
  }: {
    /**
     * App slug. Lowercase letters, digits, hyphens; must start and end with alnum.
     */
    slug: string,
    /**
     * Either a prebuilt OCI image reference (`application/json`) or a source tarball upload (`multipart/form-data`). See the operation description for plan size caps.
     */
    requestBody: CreateDeploymentRequest,
    /**
     * Idempotency key for the POST. Stored for 24h. On replay the server
     * returns the original response with `Idempotent-Replayed: true`.
     *
     */
    idempotencyKey?: string,
  }): CancelablePromise<DeploymentResponse> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/v1/apps/{slug}/deployments',
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
        403: `code: image_egress_denied — registry is in RFC1918 / IMDS / link-local, or blocked egress range.`,
        413: `code: source_too_large`,
        422: `code: deploy_failed | image_not_found | image_manifest_invalid | build_oom | build_timeout | stateless_only_violation`,
        429: `429. Two response shapes:
        - \`application/problem+json\` for code-driven 429s (\`plan_limit_concurrency\`, \`quota_exhausted\`).
        - \`text/plain\` for the authlimiter middleware (\`pkg/middleware/authlimit.go\`).
        `,
      },
    });
  }
  /**
   * Roll back to the previous deployment.
   * @returns DeploymentResponse The deployment that was created by rolling back to the previous version.
   * @throws ApiError
   */
  public static rollbackApp({
    slug,
    idempotencyKey,
  }: {
    /**
     * App slug. Lowercase letters, digits, hyphens; must start and end with alnum.
     */
    slug: string,
    /**
     * Idempotency key for the POST. Stored for 24h. On replay the server
     * returns the original response with `Idempotent-Replayed: true`.
     *
     */
    idempotencyKey?: string,
  }): CancelablePromise<DeploymentResponse> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/v1/apps/{slug}/rollback',
      path: {
        'slug': slug,
      },
      headers: {
        'Idempotency-Key': idempotencyKey,
      },
      errors: {
        401: `code: unauthorized`,
        409: `code: no_rollback_target — there is no superseded deployment to roll back to.`,
        429: `429. Two response shapes:
        - \`application/problem+json\` for code-driven 429s (\`plan_limit_concurrency\`, \`quota_exhausted\`).
        - \`text/plain\` for the authlimiter middleware (\`pkg/middleware/authlimit.go\`).
        `,
      },
    });
  }
  /**
   * List deployments across all apps on the account.
   * Paged backwards (newest first). `next_before` is an opaque cursor
   * (RFC3339Nano of the `created_at`); pass it on the next request to
   * page backwards. Empty `next_before` means end of list.
   *
   * @returns DeploymentListResponse A paginated list of deployments.
   * @throws ApiError
   */
  public static listDeployments({
    limit = 50,
    before,
  }: {
    /**
     * Page size (1–200, default 50).
     */
    limit?: number,
    /**
     * RFC3339Nano cursor from a previous response's `next_before`.
     */
    before?: string,
  }): CancelablePromise<DeploymentListResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/v1/deployments',
      query: {
        'limit': limit,
        'before': before,
      },
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
   * Fetch one deployment.
   * @returns DeploymentResponse The deployment.
   * @throws ApiError
   */
  public static getDeployment({
    id,
  }: {
    /**
     * 32-hex-char opaque ID (NOT canonical UUID).
     */
    id: string,
  }): CancelablePromise<DeploymentResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/v1/deployments/{id}',
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
   * Set the per-deployment cold-wake floor.
   * Update the deployment's min_instances (issue #557 closure /
   * ADR-072). The only mutable field on a deployment post-create;
   * image / digest / overrides / sidecars stay immutable (a new
   * deployment is the canonical way to change them). Pass
   * min_instances=0 to inherit from the parent app's floor.
   * Validated against the parent app's plan MaxMinInstances cap.
   *
   * @returns DeploymentResponse The updated deployment.
   * @throws ApiError
   */
  public static updateDeploymentMinInstances({
    id,
    requestBody,
  }: {
    /**
     * 32-hex-char opaque ID (NOT canonical UUID).
     */
    id: string,
    requestBody: {
      /**
       * Per-deployment cold-wake floor override. 0 = inherit from parent app.
       */
      min_instances: number;
    },
  }): CancelablePromise<DeploymentResponse> {
    return __request(OpenAPI, {
      method: 'PATCH',
      url: '/v1/deployments/{id}',
      path: {
        'id': id,
      },
      body: requestBody,
      mediaType: 'application/json',
      errors: {
        400: `\`400 Bad Request\` — request body failed JSON decode or
        \`min_instances\` was outside the inclusive \`[0, plan_cap]\`
        range. Stable code \`min_instances_invalid\`.
        `,
        401: `code: unauthorized`,
        404: `code: not_found`,
        422: `\`422 Unprocessable Entity\` — request was syntactically valid
        but the parent app's plan refuses the override (e.g. a
        Free app PATCHing \`min_instances=1\`). Stable code
        \`plan_min_instances_not_allowed\`.
        `,
        429: `429. Two response shapes:
        - \`application/problem+json\` for code-driven 429s (\`plan_limit_concurrency\`, \`quota_exhausted\`).
        - \`text/plain\` for the authlimiter middleware (\`pkg/middleware/authlimit.go\`).
        `,
      },
    });
  }
  /**
   * Stream build logs (SSE).
   * Server-Sent Events stream of build logs. `follow=1` holds the
   * connection open until the build completes.
   *
   * @returns any A text/event-stream of build log entries, terminated by an empty SSE frame when the build finishes.
   * @throws ApiError
   */
  public static streamDeploymentLogs({
    id,
    beforeSeq,
    limit,
    follow = 0,
  }: {
    /**
     * 32-hex-char opaque ID (NOT canonical UUID).
     */
    id: string,
    /**
     * Stream cursor — return only log entries with seq strictly less than this value.
     */
    beforeSeq?: number,
    /**
     * Maximum number of log entries to return in the initial burst before streaming.
     */
    limit?: number,
    /**
     * If 1, hold the connection open and stream new build log entries as they arrive.
     */
    follow?: 0 | 1,
  }): CancelablePromise<any> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/v1/deployments/{id}/logs',
      path: {
        'id': id,
      },
      query: {
        'before_seq': beforeSeq,
        'limit': limit,
        'follow': follow,
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
   * Get build provenance.
   * Returns the ADR-038 `build_provenance` row for a single build.
   * Each successful build produces exactly one provenance row
   * (builderd's populator runs at the `markSucceeded` sites); the
   * row is the customer-visible "what ran?" record: buildkit /
   * railpack version, base / runner digests, source URL + commit
   * SHA, plan, builder node ID, and the build's started_at /
   * finished_at timestamps.
   *
   * A 404 with `code=build_provenance_not_found` is returned when
   * the build exists but no provenance row landed (the populator
   * logs a WARN inside builderd on a failed INSERT — the build
   * itself still succeeded). A 404 with `code=not_found` is
   * returned when no build row matches the id, or when the
   * build's owning app belongs to a different account.
   *
   * @returns BuildProvenanceResponse The build provenance row, with every field populated. Empty strings indicate a column the populator hasn't filled yet (the schema half of Phase 2 is in this PR; cosign / SBOM populate the rest in Phase 3).
   * @throws ApiError
   */
  public static getBuildProvenance({
    id,
  }: {
    /**
     * 32-hex-char opaque ID (NOT canonical UUID).
     */
    id: string,
  }): CancelablePromise<BuildProvenanceResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/v1/builds/{id}/provenance',
      path: {
        'id': id,
      },
      errors: {
        401: `code: unauthorized`,
        404: `Either the build row is missing, OR the build exists but the populator INSERT failed (code=build_provenance_not_found).`,
        429: `429. Two response shapes:
        - \`application/problem+json\` for code-driven 429s (\`plan_limit_concurrency\`, \`quota_exhausted\`).
        - \`text/plain\` for the authlimiter middleware (\`pkg/middleware/authlimit.go\`).
        `,
      },
    });
  }
  /**
   * Get build SBOM (CycloneDX JSON).
   * Streams the CycloneDX 1.5 SBOM JSON generated by `imaged`'s
   * post-build syft pass (issue #299). The body is the raw SBOM
   * document; the response content-type is
   * `application/vnd.cyclonedx+json` so external tooling
   * (cyclonedx-cli validate, jq with `@cyclonedx-json`, Grype's
   * `--from-file=cyclonedx-json`) can dispatch on header alone.
   *
   * Three failure modes, each with its own code so the SDK and
   * `faas build sbom <id>` can branch:
   *
   * - `404 not_found` — the build id does not exist, OR the build
   * belongs to another account (the handler returns the same
   * surface on both so account-existence isn't probeable).
   * - `503 build_sbom_unavailable` — the build row exists for this
   * account but imaged's syft populator did not persist a
   * CycloneDX document (pre-PR build, or best-effort WARN).
   * The CLI prints "no SBOM for this build" and exits 1; the
   * operator's job is to re-deploy imaged with Phase 3 active.
   * - `503 capacity_unavailable` — the SBOM exists but the file
   * was unreadable from disk (storage backend returned a
   * transient error). The customer retries once.
   *
   * @returns any The CycloneDX SBOM JSON document, served verbatim from the storage backend.
   * @throws ApiError
   */
  public static getBuildSbom({
    id,
  }: {
    /**
     * 32-hex-char opaque ID (NOT canonical UUID).
     */
    id: string,
  }): CancelablePromise<Record<string, any>> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/v1/builds/{id}/sbom',
      path: {
        'id': id,
      },
      errors: {
        401: `code: unauthorized`,
        404: `Either the build row is missing, OR the build exists but belongs to a different account (code=not_found on every negative path so account-existence isn't probeable).`,
        429: `429. Two response shapes:
        - \`application/problem+json\` for code-driven 429s (\`plan_limit_concurrency\`, \`quota_exhausted\`).
        - \`text/plain\` for the authlimiter middleware (\`pkg/middleware/authlimit.go\`).
        `,
        503: `The SBOM populator has not produced an artefact for this build (code=build_sbom_unavailable), or the storage backend failed (code=capacity_unavailable).`,
      },
    });
  }
}

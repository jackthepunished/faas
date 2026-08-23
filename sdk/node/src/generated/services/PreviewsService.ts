/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { CancelablePromise } from '../core/CancelablePromise.js';
import { OpenAPI } from '../core/OpenAPI.js';
import { request as __request } from '../core/request.js';
export class PreviewsService {
  /**
   * Tear down a preview app.
   * One-click destroy of a preview app row (issue #961 Mega-C PR-1,
   * leaf 3). Typically fired from a "Tear down this preview" link
   * posted to the GitHub PR by `pkg/githubd`. Distinct from
   * `DELETE /v1/apps/{slug}` because the preview teardown also
   * stamps `apps.preview_pr_state='torn_down'` so the janitor
   * doesn't re-process the row on a subsequent tick, and emits a
   * distinct audit kind (`preview.destroyed_by_customer` vs
   * `app.deleted`).
   *
   * @returns void
   * @throws ApiError
   */
  public static destroyPreview({
    slug,
  }: {
    /**
     * App slug. Lowercase letters, digits, hyphens; must start and end with alnum.
     */
    slug: string,
  }): CancelablePromise<void> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/v1/preview/{slug}/destroy',
      path: {
        'slug': slug,
      },
      errors: {
        401: `code: unauthorized`,
        404: `404 — slug does not identify a preview app. Use DELETE /v1/apps/{slug} to destroy a production app.`,
        429: `429. Two response shapes:
        - \`application/problem+json\` for code-driven 429s (\`plan_limit_concurrency\`, \`quota_exhausted\`).
        - \`text/plain\` for the authlimiter middleware (\`pkg/middleware/authlimit.go\`).
        `,
      },
    });
  }
}

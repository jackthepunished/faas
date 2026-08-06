/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { AccountDeletionResponse } from '../models/AccountDeletionResponse.js';
import type { AccountExportResponse } from '../models/AccountExportResponse.js';
import type { AccountResponse } from '../models/AccountResponse.js';
import type { ChangePlanRequest } from '../models/ChangePlanRequest.js';
import type { RaiseOverageCapRequest } from '../models/RaiseOverageCapRequest.js';
import type { CancelablePromise } from '../core/CancelablePromise.js';
import { OpenAPI } from '../core/OpenAPI.js';
import { request as __request } from '../core/request.js';
export class AccountService {
  /**
   * Whoami.
   * Returns the calling account, its plan, and quota limits.
   * @returns AccountResponse The calling account: id, plan, limits snapshot, current-month usage, and total app count.
   * @throws ApiError
   */
  public static getAccount(): CancelablePromise<AccountResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/v1/account',
      errors: {
        401: `code: unauthorized`,
        402: `code: billing_past_due — account is suspended; pay invoice to resume.`,
        429: `429. Two response shapes:
        - \`application/problem+json\` for code-driven 429s (\`plan_limit_concurrency\`, \`quota_exhausted\`).
        - \`text/plain\` for the authlimiter middleware (\`pkg/middleware/authlimit.go\`).
        `,
      },
    });
  }
  /**
   * Stage account deletion (30-day grace).
   * Stages deletion. The account becomes `deleted_pending` for 30 days
   * during which the customer can call `POST /v1/account/restore`. After
   * the grace period, all rows are GC'd.
   *
   * @returns AccountDeletionResponse Staged.
   * @throws ApiError
   */
  public static deleteAccount({
    idempotencyKey,
  }: {
    /**
     * Idempotency key for the POST. Stored for 24h. On replay the server
     * returns the original response with `Idempotent-Replayed: true`.
     *
     */
    idempotencyKey?: string,
  }): CancelablePromise<AccountDeletionResponse> {
    return __request(OpenAPI, {
      method: 'DELETE',
      url: '/v1/account',
      headers: {
        'Idempotency-Key': idempotencyKey,
      },
      errors: {
        401: `code: unauthorized`,
        409: `code: account_deletion_confirm_required | account_deletion_pending | account_not_restorable`,
      },
    });
  }
  /**
   * Change billing plan.
   * Switch the account between `free`, `hobby`, `pro`, and `scale`. The
   * Stripe subscription is updated server-side; the response carries the
   * new plan only.
   *
   * @returns AccountResponse The updated account profile after the plan change.
   * @throws ApiError
   */
  public static changePlan({
    requestBody,
  }: {
    /**
     * Plan change payload.
     */
    requestBody: ChangePlanRequest,
  }): CancelablePromise<AccountResponse> {
    return __request(OpenAPI, {
      method: 'PATCH',
      url: '/v1/account/plan',
      body: requestBody,
      mediaType: 'application/json',
      errors: {
        400: `code: validation_failed | source_invalid | build_undetected | handler_missing | image_required | cron_invalid | secret_invalid_key`,
        401: `code: unauthorized`,
        402: `code: billing_past_due — account is suspended; pay invoice to resume.`,
        409: `code: conflict`,
        429: `429. Two response shapes:
        - \`application/problem+json\` for code-driven 429s (\`plan_limit_concurrency\`, \`quota_exhausted\`).
        - \`text/plain\` for the authlimiter middleware (\`pkg/middleware/authlimit.go\`).
        `,
      },
    });
  }
  /**
   * Export full account data (GDPR).
   * Returns a JSON bundle containing every resource the account owns
   * (apps, deployments, builds, instances, usage, domains, crons, API
   * keys, app secrets) plus the GDPR audit trail. Available to
   * `deleted_pending` accounts so the customer can take a final export
   * during the 30-day grace window.
   *
   * @returns AccountExportResponse A bundled JSON document: the account itself plus every owned app, deployment, build, instance, usage record, domain, cron, API key, sealed-secret envelope, and the audit trail.
   * @throws ApiError
   */
  public static exportAccount({
    includeSecrets = true,
  }: {
    /**
     * If true (default), include the sealed-secret envelopes.
     */
    includeSecrets?: boolean,
  }): CancelablePromise<AccountExportResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/v1/account/export',
      query: {
        'include_secrets': includeSecrets,
      },
      errors: {
        401: `code: unauthorized`,
      },
    });
  }
  /**
   * Restore a `deleted_pending` account.
   * Cancels a staged deletion. Returns 409 `account_not_restorable` if the 30-day grace has elapsed.
   * @returns AccountResponse Restored.
   * @throws ApiError
   */
  public static restoreAccount(): CancelablePromise<AccountResponse> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/v1/account/restore',
      errors: {
        401: `code: unauthorized`,
        409: `code: account_deletion_confirm_required | account_deletion_pending | account_not_restorable`,
      },
    });
  }
  /**
   * Set or clear the account's spend cap (issue
   * Sets accounts.overage_cap_cents in integer cents. Body shape:
   *
   * {"overage_cap_cents": <int|null>}
   *
   * Pass `null` (or omit the field) to clear the cap (NULL).
   * Pass 0 to set "no overage allowed." Passing a positive integer
   * sets the monthly ceiling. The migration CHECK constraint at
   * `migrations/00054_account_credits.sql` pins
   * `overage_cap_cents IS NULL OR overage_cap_cents >= 0`; a
   * negative value is rejected at the apid validator before the
   * store ever sees it, returning 400 `validation_failed`.
   *
   * Once current-month overage meets/exceeds the cap, schedd refuses
   * new wakes with `code: admission_refused` (HTTP 402). The cap is
   * account-self-scoped (no admin scope required) and the response
   * is the post-update account state. Audit row
   * `overage.cap_changed` is emitted on every successful call.
   *
   * @returns AccountResponse The updated account.
   * @throws ApiError
   */
  public static raiseOverageCap({
    requestBody,
    idempotencyKey,
  }: {
    requestBody: RaiseOverageCapRequest,
    /**
     * Idempotency key for the POST. Stored for 24h. On replay the server
     * returns the original response with `Idempotent-Replayed: true`.
     *
     */
    idempotencyKey?: string,
  }): CancelablePromise<AccountResponse> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/v1/account/overage-cap',
      headers: {
        'Idempotency-Key': idempotencyKey,
      },
      body: requestBody,
      mediaType: 'application/json',
      errors: {
        400: `code: validation_failed | source_invalid | build_undetected | handler_missing | image_required | cron_invalid | secret_invalid_key`,
        401: `code: unauthorized`,
      },
    });
  }
}

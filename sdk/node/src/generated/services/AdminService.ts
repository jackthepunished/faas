/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { AccountCreditResponse } from '../models/AccountCreditResponse.js';
import type { ConsumeInvoiceResponse } from '../models/ConsumeInvoiceResponse.js';
import type { CancelablePromise } from '../core/CancelablePromise.js';
import { OpenAPI } from '../core/OpenAPI.js';
import { request as __request } from '../core/request.js';
export class AdminService {
  /**
   * Issue a positive-cents credit to an account (admin-only).
   * @returns AccountCreditResponse Credit issued. Returns the new credit row.
   * @throws ApiError
   */
  public static issueAccountCredit({
    id,
    requestBody,
  }: {
    /**
     * Target account UUID.
     */
    id: string,
    requestBody: {
      /**
       * Credit amount in EUR cents (integer).
       */
      cents: number;
      /**
       * Operator-supplied audit reason.
       */
      reason: string;
    },
  }): CancelablePromise<AccountCreditResponse> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/v1/admin/accounts/{id}/credits',
      path: {
        'id': id,
      },
      body: requestBody,
      mediaType: 'application/json',
      errors: {
        400: `code: validation_failed | source_invalid | build_undetected | handler_missing | image_required | cron_invalid | secret_invalid_key`,
        401: `code: unauthorized`,
        403: `code: admin_required — call requires a Bearer with the admin scope AND an email in FAAS_ADMIN_EMAILS.`,
        404: `code: not_found`,
        429: `429. Two response shapes:
        - \`application/problem+json\` for code-driven 429s (\`plan_limit_concurrency\`, \`quota_exhausted\`).
        - \`text/plain\` for the authlimiter middleware (\`pkg/middleware/authlimit.go\`).
        `,
      },
    });
  }
  /**
   * Drain active credits FIFO against an invoice's overage (admin-only, MFA-gated).
   * @returns ConsumeInvoiceResponse Reducer ran (or replayed). consumed_cents is the floored integer cents drained against this invoice.
   * @throws ApiError
   */
  public static consumeInvoiceCredits({
    id,
  }: {
    /**
     * Invoice row UUID from GET /v1/invoices.
     */
    id: string,
  }): CancelablePromise<ConsumeInvoiceResponse> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/v1/invoices/{id}/consume-credits',
      path: {
        'id': id,
      },
      errors: {
        400: `code: validation_failed | source_invalid | build_undetected | handler_missing | image_required | cron_invalid | secret_invalid_key`,
        401: `code: unauthorized`,
        403: `code: admin_required — call requires an admin-scoped Bearer with the caller email in FAAS_ADMIN_EMAILS AND a verified MFA factor.`,
        404: `code: not_found`,
        429: `429. Two response shapes:
        - \`application/problem+json\` for code-driven 429s (\`plan_limit_concurrency\`, \`quota_exhausted\`).
        - \`text/plain\` for the authlimiter middleware (\`pkg/middleware/authlimit.go\`).
        `,
      },
    });
  }
}

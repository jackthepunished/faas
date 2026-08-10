/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { AccountCreditResponse } from '../models/AccountCreditResponse.js';
import type { BillingCatalogResponse } from '../models/BillingCatalogResponse.js';
import type { BillingReconcileResponse } from '../models/BillingReconcileResponse.js';
import type { ConsumeInvoiceResponse } from '../models/ConsumeInvoiceResponse.js';
import type { RekeyProgress } from '../models/RekeyProgress.js';
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
  /**
   * Read the cached Paddle price + product catalog (admin-only).
   * Returns the in-memory catalog snapshot from *paddle.Provider.
   * synced_at is the timestamp of the most recent successful
   * EnsurePlanProducts call; an empty string means no hydration
   * has run yet. On a Stripe deployment the handler returns 501
   * with code billing_op_unsupported — the type assertion at
   * the dispatcher fails and the surface is provider-scoped.
   *
   * @returns BillingCatalogResponse Catalog snapshot. Entries may be empty if no hydration has run.
   * @throws ApiError
   */
  public static listPaddleCatalog(): CancelablePromise<BillingCatalogResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/v1/admin/billing-paddle-catalog',
      errors: {
        401: `code: unauthorized`,
        403: `code: admin_required — GET /v1/admin/billing-paddle-catalog requires a Bearer with the admin scope AND an email in FAAS_ADMIN_EMAILS.`,
        429: `429. Two response shapes:
        - \`application/problem+json\` for code-driven 429s (\`plan_limit_concurrency\`, \`quota_exhausted\`).
        - \`text/plain\` for the authlimiter middleware (\`pkg/middleware/authlimit.go\`).
        `,
        501: `code: billing_op_unsupported — active provider does not implement the paddle.OpProvider surface.`,
      },
    });
  }
  /**
   * Signal a Paddle catalog reset (admin-only).
   * Paddle's catalog is durable on the platform — the in-memory
   * reset is a no-op (returns nil immediately) and the handler
   * renders a 200 with empty entries so the CLI can print the
   * "delete products from the Paddle Dashboard, then call
   * sync" message. Future work (issue #279+) may add
   * merchant-side cleanup; this handler will then return 502
   * on SDK failure rather than 200.
   *
   * @returns BillingCatalogResponse Reset signal recorded. Always returns empty entries.
   * @throws ApiError
   */
  public static resetPaddleCatalog(): CancelablePromise<BillingCatalogResponse> {
    return __request(OpenAPI, {
      method: 'DELETE',
      url: '/v1/admin/billing-paddle-catalog',
      errors: {
        401: `code: unauthorized`,
        403: `code: admin_required — DELETE /v1/admin/billing-paddle-catalog requires a Bearer with the admin scope AND an email in FAAS_ADMIN_EMAILS.`,
        429: `429. Two response shapes:
        - \`application/problem+json\` for code-driven 429s (\`plan_limit_concurrency\`, \`quota_exhausted\`).
        - \`text/plain\` for the authlimiter middleware (\`pkg/middleware/authlimit.go\`).
        `,
        501: `code: billing_op_unsupported — provider does not implement paddle.OpProvider.`,
      },
    });
  }
  /**
   * Force a Paddle catalog hydration (admin-only).
   * Idempotent: re-running on the same platform hits the
   * Status=active filter on ListProducts, finds existing
   * products/prices, and skips POST. Idempotency-Key middleware
   * replays the same 200 for a flaky-network retry so the SDK
   * round-trip is not re-issued. Returns the post-sync catalog.
   *
   * @returns BillingCatalogResponse Post-sync catalog. synced_at is "now" by construction.
   * @throws ApiError
   */
  public static syncPaddleCatalog(): CancelablePromise<BillingCatalogResponse> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/v1/admin/billing-paddle-catalog/sync',
      errors: {
        401: `code: unauthorized`,
        403: `code: admin_required — POST /v1/admin/billing-paddle-catalog/sync requires a Bearer with the admin scope AND an email in FAAS_ADMIN_EMAILS.`,
        429: `429. Two response shapes:
        - \`application/problem+json\` for code-driven 429s (\`plan_limit_concurrency\`, \`quota_exhausted\`).
        - \`text/plain\` for the authlimiter middleware (\`pkg/middleware/authlimit.go\`).
        `,
        501: `code: billing_op_unsupported — active provider does not advertise paddle.OpProvider; the type assertion at the dispatcher fails.`,
        502: `code: billing_sync_failed — Paddle SDK round-trip failed.`,
      },
    });
  }
  /**
   * Run a single-account reconcile against the active billing Provider (admin-only).
   * Loads the account, calls billing.Provider.ReconcileUsage for
   * a rolling 30-day window [start, end). Stripe implements
   * this (ADR-049 §B.1); Paddle returns billing.ErrNotImplemented
   * and the handler maps to 501. The response surfaces the
   * SDK-returned mb_seconds total so an operator can diff
   * against the local usage_minutes sum.
   *
   * @returns BillingReconcileResponse Reconcile ran. mb_seconds is the SDK-returned total for [start, end).
   * @throws ApiError
   */
  public static reconcileAccount({
    id,
  }: {
    /**
     * Account UUID whose 30-day reconcile window the operator wants to inspect.
     */
    id: string,
  }): CancelablePromise<BillingReconcileResponse> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/v1/admin/billing-reconcile/{id}',
      path: {
        'id': id,
      },
      errors: {
        400: `code: validation_failed | source_invalid | build_undetected | handler_missing | image_required | cron_invalid | secret_invalid_key`,
        401: `code: unauthorized`,
        403: `code: admin_required — POST /v1/admin/billing-reconcile/{id} requires a Bearer with the admin scope AND an email in FAAS_ADMIN_EMAILS.`,
        404: `code: not_found`,
        429: `429. Two response shapes:
        - \`application/problem+json\` for code-driven 429s (\`plan_limit_concurrency\`, \`quota_exhausted\`).
        - \`text/plain\` for the authlimiter middleware (\`pkg/middleware/authlimit.go\`).
        `,
        501: `code: billing_reconcile_unsupported — provider does not implement ReconcileUsage.`,
        502: `code: billing_reconcile_failed — SDK round-trip failed.`,
      },
    });
  }
  /**
   * Read the cumulative rekey walk progress (admin-only).
   * Returns the latest RekeyProgress snapshot the apid rekey
   * runner has written — either to the in-process atomic pointer
   * (memory-only mode) or to FAAS_REKEY_PROGRESS_FILE on disk.
   * Operators poll this endpoint to monitor the walk after a
   * host identity rotation; the response shape mirrors
   * rekey.RekeyProgress exactly so a future operator tool
   * (e.g. `gregale rekey status`) can decode without a parallel
   * type.
   *
   * `total` is the running count of rows observed so far; it can
   * grow as the walk paginates through (account_id, app_id, key)
   * order. `rekeyed` + `skipped` should approach `total` once
   * the walk drains. `failed` should stay at zero; a non-zero
   * value means the unseal step threw for at least one row —
   * the operationally safe recovery is `git rm
   * migrations*reserve_slot.sql`-style idempotent re-trigger
   * (toggle FAAS_REKEY_ENABLED and restart apid; the seen-set
   * inside Replayer dedupes already-done rows).
   *
   * When the runner is disabled (FAAS_REKEY_ENABLED unset), the
   * endpoint returns 503 with code `rekey_disabled` so an
   * operator can distinguish "no work yet" from "feature off".
   *
   * @returns RekeyProgress Current rekey progress snapshot.
   * @throws ApiError
   */
  public static getRekeyProgress(): CancelablePromise<RekeyProgress> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/v1/admin/secrets/rekey-progress',
      errors: {
        401: `code: unauthorized`,
        403: `code: admin_required — GET /v1/admin/secrets/rekey-progress requires a Bearer with the admin scope AND an email in FAAS_ADMIN_EMAILS.`,
        429: `429. Two response shapes:
        - \`application/problem+json\` for code-driven 429s (\`plan_limit_concurrency\`, \`quota_exhausted\`).
        - \`text/plain\` for the authlimiter middleware (\`pkg/middleware/authlimit.go\`).
        `,
        503: `code: rekey_disabled — FAAS_REKEY_ENABLED is unset on this apid; the background re-seal runner is not running. Set the env flag and restart apid to opt in.`,
      },
    });
  }
}

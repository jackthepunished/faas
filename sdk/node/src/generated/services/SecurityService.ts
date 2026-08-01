/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { AddTrustedSignerRequest } from '../models/AddTrustedSignerRequest.js';
import type { AppSecurityRequest } from '../models/AppSecurityRequest.js';
import type { AppSecurityResponse } from '../models/AppSecurityResponse.js';
import type { AppTrustedSignerListResponse } from '../models/AppTrustedSignerListResponse.js';
import type { TrustedSigner } from '../models/TrustedSigner.js';
import type { CancelablePromise } from '../core/CancelablePromise.js';
import { OpenAPI } from '../core/OpenAPI.js';
import { request as __request } from '../core/request.js';
export class SecurityService {
  /**
   * Toggle the require_signed flag for an app (admin + MFA).
   * Operator-only surface for the per-app cosign signature-enforcement
   * flag (issue #472 / ADR-054). Mounted with `authLimited → requireMFA →
   * requireScope(ScopesAdminOnly...)`. The customer PATCH /v1/apps/{slug}
   * endpoint silently drops the field — flipping it through that surface
   * is a no-op — so the only path that persists `require_signed=true` is
   * this one.
   *
   * `nil` = no field set (no-op 200). Non-nil = atomic overwrite.
   *
   * Audit event: `app.security_updated` carries old/new values.
   *
   * @returns AppSecurityResponse The updated flag state.
   * @throws ApiError
   */
  public static patchAppSecurity({
    slug,
    requestBody,
  }: {
    /**
     * App slug. Lowercase letters, digits, hyphens; must start and end with alnum.
     */
    slug: string,
    requestBody: AppSecurityRequest,
  }): CancelablePromise<AppSecurityResponse> {
    return __request(OpenAPI, {
      method: 'PATCH',
      url: '/v1/apps/{slug}/security',
      path: {
        'slug': slug,
      },
      body: requestBody,
      mediaType: 'application/json',
      errors: {
        400: `code: validation_failed | source_invalid | build_undetected | handler_missing | image_required | cron_invalid | secret_invalid_key`,
        401: `code: unauthorized`,
        403: `code: forbidden — caller is authenticated but lacks the required scope, OR plan_limit_trusted_signers / plan_limit_secret / etc. when the resource count would exceed the plan cap.`,
        404: `code: app_not_found — slug does not exist for the authenticated account.`,
        500: `code: capacity — server-side error; retry with backoff.`,
      },
    });
  }
  /**
   * List the per-app trusted-publisher list (admin).
   * Lists every (signer_name, public_key_pem) row for this app
   * (issue #472 / ADR-054). Admin-scoped; the wire form is the
   * base64-encoded DER SPKI bytes, NOT a PEM-armoured block.
   * Empty list is the EXPECTED state for any app with
   * require_signed=false. Mounted with `authLimited → requireMFA →
   * requireScope(ScopesAdminOnly...)`.
   *
   * @returns AppTrustedSignerListResponse The trusted-publisher list (may be empty).
   * @throws ApiError
   */
  public static listAppTrustedSigners({
    slug,
  }: {
    /**
     * App slug. Lowercase letters, digits, hyphens; must start and end with alnum.
     */
    slug: string,
  }): CancelablePromise<AppTrustedSignerListResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/v1/apps/{slug}/trusted_signers',
      path: {
        'slug': slug,
      },
      errors: {
        401: `code: unauthorized`,
        403: `code: forbidden — caller is authenticated but lacks the required scope, OR plan_limit_trusted_signers / plan_limit_secret / etc. when the resource count would exceed the plan cap.`,
        404: `code: app_not_found — slug does not exist for the authenticated account.`,
        500: `code: capacity — server-side error; retry with backoff.`,
      },
    });
  }
  /**
   * Onboard (or replace) a trusted publisher (admin + MFA).
   * Writes the (app_id, signer_name) row with the supplied base64-DER
   * blob (issue #472 / ADR-054). apid decodes the blob and persists it
   * verbatim; imaged mirrors the row to `/etc/faas/secrets/trusted-publishers/{name}.pem`
   * and refreshes its in-memory trust cache on `pg_notify('trusted_signer_changed')`.
   * PUT semantics: idempotent re-PUT replaces the previous blob.
   *
   * Audit event: `app.trusted_signer_added`.
   *
   * @returns TrustedSigner The onboarded trusted signer.
   * @throws ApiError
   */
  public static putAppTrustedSigner({
    slug,
    name,
    requestBody,
  }: {
    /**
     * App slug. Lowercase letters, digits, hyphens; must start and end with alnum.
     */
    slug: string,
    /**
     * Lower-case signer label. Matches the on-disk filename (without .pem) under /etc/faas/secrets/trusted-publishers/.
     */
    name: string,
    requestBody: AddTrustedSignerRequest,
  }): CancelablePromise<TrustedSigner> {
    return __request(OpenAPI, {
      method: 'PUT',
      url: '/v1/apps/{slug}/trusted_signers/{name}',
      path: {
        'slug': slug,
        'name': name,
      },
      body: requestBody,
      mediaType: 'application/json',
      errors: {
        400: `trusted_signer_invalid (bytes out of [64,1024] range, non-ECDSA, non-P-256, etc).`,
        401: `code: unauthorized`,
        403: `Forbidden — admin scope required, OR plan_limit_trusted_signers when the per-app count would exceed the plan cap.`,
        404: `code: app_not_found — slug does not exist for the authenticated account.`,
        500: `code: capacity — server-side error; retry with backoff.`,
      },
    });
  }
  /**
   * Offboard a trusted publisher (admin + MFA).
   * Deletes the (app_id, signer_name) row. 404 returns the canonical
   * `trusted_signer_not_found` Problem so `gregale trusted-publishers remove`
   * can treat absent rows as idempotent success.
   *
   * Audit event: `app.trusted_signer_removed`.
   *
   * @returns void
   * @throws ApiError
   */
  public static deleteAppTrustedSigner({
    slug,
    name,
  }: {
    /**
     * App slug. Lowercase letters, digits, hyphens; must start and end with alnum.
     */
    slug: string,
    /**
     * Lower-case signer label. Matches the on-disk filename (without .pem) under /etc/faas/secrets/trusted-publishers/.
     */
    name: string,
  }): CancelablePromise<void> {
    return __request(OpenAPI, {
      method: 'DELETE',
      url: '/v1/apps/{slug}/trusted_signers/{name}',
      path: {
        'slug': slug,
        'name': name,
      },
      errors: {
        401: `code: unauthorized`,
        403: `code: forbidden — caller is authenticated but lacks the required scope, OR plan_limit_trusted_signers / plan_limit_secret / etc. when the resource count would exceed the plan cap.`,
        404: `trusted_signer_not_found — the (slug, name) row does not exist.`,
        500: `code: capacity — server-side error; retry with backoff.`,
      },
    });
  }
}

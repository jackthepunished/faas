/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { CreateCustomDomainRequest } from '../models/CreateCustomDomainRequest.js';
import type { CustomDomainResponse } from '../models/CustomDomainResponse.js';
import type { DomainDoctorReport } from '../models/DomainDoctorReport.js';
import type { CancelablePromise } from '../core/CancelablePromise.js';
import { OpenAPI } from '../core/OpenAPI.js';
import { request as __request } from '../core/request.js';
export class DomainsService {
  /**
   * List custom domain bindings.
   * @returns CustomDomainResponse Custom-domain bindings on the account.
   * @throws ApiError
   */
  public static listDomains(): CancelablePromise<Array<CustomDomainResponse>> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/v1/domains',
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
   * Bind a custom domain.
   * @returns CustomDomainResponse The new custom-domain binding.
   * @throws ApiError
   */
  public static createDomain({
    requestBody,
    idempotencyKey,
  }: {
    /**
     * Domain-bind payload — domain string + target app slug. See CreateCustomDomainRequest.
     */
    requestBody: CreateCustomDomainRequest,
    /**
     * Idempotency key for the POST. Stored for 24h. On replay the server
     * returns the original response with `Idempotent-Replayed: true`.
     *
     */
    idempotencyKey?: string,
  }): CancelablePromise<CustomDomainResponse> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/v1/domains',
      headers: {
        'Idempotency-Key': idempotencyKey,
      },
      body: requestBody,
      mediaType: 'application/json',
      errors: {
        400: `code: validation_failed | source_invalid | build_undetected | handler_missing | image_required | cron_invalid | secret_invalid_key`,
        401: `code: unauthorized`,
        409: `code: conflict`,
        429: `429. Two response shapes:
        - \`application/problem+json\` for code-driven 429s (\`plan_limit_concurrency\`, \`quota_exhausted\`).
        - \`text/plain\` for the authlimiter middleware (\`pkg/middleware/authlimit.go\`).
        `,
      },
    });
  }
  /**
   * Show a custom domain's cert details (issue
   * Returns the durable domain row + the live cert chain
   * (NotAfter, SANs) by dialing port-443 and reading the leaf
   * cert. Used by `gregale domains show <domain>`.
   *
   * @returns CustomDomainResponse The domain row + cert details.
   * @throws ApiError
   */
  public static getDomain({
    domain,
  }: {
    /**
     * The custom domain string (e.g. `app.example.com`).
     */
    domain: string,
  }): CancelablePromise<CustomDomainResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/v1/domains/{domain}',
      path: {
        'domain': domain,
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
   * Remove a custom domain binding.
   * @returns void
   * @throws ApiError
   */
  public static deleteDomain({
    domain,
  }: {
    /**
     * The custom domain string (e.g. `app.example.com`).
     */
    domain: string,
  }): CancelablePromise<void> {
    return __request(OpenAPI, {
      method: 'DELETE',
      url: '/v1/domains/{domain}',
      path: {
        'domain': domain,
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
   * Re-verify a domain's DNS + cert (issue
   * Re-runs the DNS verifier + cert dial; returns the canonical
   * CustomDomainResponse. Used by `gregale domains verify
   * <domain>`. Idempotent: POSTing twice does not change the
   * durable verification state.
   *
   * @returns CustomDomainResponse The row + cert details. `cert_not_after` / `cert_sans` populated when the cert dial succeeds.
   * @throws ApiError
   */
  public static verifyDomain({
    domain,
    idempotencyKey,
  }: {
    /**
     * The custom domain to re-verify (e.g. `app.example.com`). The same shape as the GET path; verify walks DNS + cert while show returns the durable row.
     */
    domain: string,
    /**
     * Idempotency key for the POST. Stored for 24h. On replay the server
     * returns the original response with `Idempotent-Replayed: true`.
     *
     */
    idempotencyKey?: string,
  }): CancelablePromise<CustomDomainResponse> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/v1/domains/{domain}/verify',
      path: {
        'domain': domain,
      },
      headers: {
        'Idempotency-Key': idempotencyKey,
      },
      errors: {
        401: `code: unauthorized`,
        404: `code: not_found`,
        422: `code: domain_verification_failed | domain_cert_not_issued.
        DNS walk found a missing/mismatched TXT record, or the
        port-443 cert is a CDN cert whose SANs do not include
        the customer's domain.
        `,
        429: `429. Two response shapes:
        - \`application/problem+json\` for code-driven 429s (\`plan_limit_concurrency\`, \`quota_exhausted\`).
        - \`text/plain\` for the authlimiter middleware (\`pkg/middleware/authlimit.go\`).
        `,
      },
    });
  }
  /**
   * Doctor a domain (ADR-120).
   * Returns the 5-check doctor report (DNS record found /
   * points to Gregale / TLS certificate / CAA permits / IPv6
   * conflict) with a human-readable remediation line per failing
   * check. Backed by GET /v1/domains/{domain}/doctor. The
   * handler reads the latest observation row from
   * domain_doctor_observations; on a stale or missing row it
   * triggers a synchronous re-probe with a 5s budget. `stale`
   * on the response is true when the cached row was older than
   * FAAS_DOMAIN_DOCTOR_TTL_SECONDS (default 300).
   *
   * @returns DomainDoctorReport The 5-check report.
   * @throws ApiError
   */
  public static domainDoctor({
    domain,
  }: {
    /**
     * The custom domain to doctor (e.g. `app.example.com`).
     */
    domain: string,
  }): CancelablePromise<DomainDoctorReport> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/v1/domains/{domain}/doctor',
      path: {
        'domain': domain,
      },
      errors: {
        401: `code: unauthorized`,
        404: `code: not_found`,
        429: `429. Two response shapes:
        - \`application/problem+json\` for code-driven 429s (\`plan_limit_concurrency\`, \`quota_exhausted\`).
        - \`text/plain\` for the authlimiter middleware (\`pkg/middleware/authlimit.go\`).
        `,
        503: `code: doctor_unavailable`,
      },
    });
  }
}

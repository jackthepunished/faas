/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { AlertRuleResponse } from '../models/AlertRuleResponse.js';
import type { CreateAlertRuleRequest } from '../models/CreateAlertRuleRequest.js';
import type { RotateAlertRuleSecretResponse } from '../models/RotateAlertRuleSecretResponse.js';
import type { UpdateAlertRuleRequest } from '../models/UpdateAlertRuleRequest.js';
import type { CancelablePromise } from '../core/CancelablePromise.js';
import { OpenAPI } from '../core/OpenAPI.js';
import { request as __request } from '../core/request.js';
export class AlertRulesService {
  /**
   * List alert rules visible at this app.
   * Returns both app-scoped rules (app_id == <slug>) and
   * account-wide rules (app_id == ""). Account-wide rules
   * apply to every app on the account.
   *
   * @returns AlertRuleResponse Alert rules on the account, filtered to those visible at this app.
   * @throws ApiError
   */
  public static listAlertRules({
    slug,
  }: {
    /**
     * App slug. Lowercase letters, digits, hyphens; must start and end with alnum.
     */
    slug: string,
  }): CancelablePromise<Array<AlertRuleResponse>> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/v1/apps/{slug}/alerts',
      path: {
        'slug': slug,
      },
      errors: {
        401: `code: unauthorized`,
        402: `code: alert_rule_invalid | plan_alert_rules_not_allowed | plan_alert_rule_quota | image_egress_denied`,
        404: `code: not_found`,
        429: `429. Two response shapes:
        - \`application/problem+json\` for code-driven 429s (\`plan_limit_concurrency\`, \`quota_exhausted\`).
        - \`text/plain\` for the authlimiter middleware (\`pkg/middleware/authlimit.go\`).
        `,
      },
    });
  }
  /**
   * Create an alert rule.
   * Plaintext webhook_secret arrives in the body, is sealed
   * via the host age recipient, and never appears in a
   * response. webhook_url is SSRF-guarded at write time
   * (loopback + metadata ranges denied).
   *
   * @returns AlertRuleResponse The new alert rule (carries the masked secret).
   * @throws ApiError
   */
  public static createAlertRule({
    slug,
    requestBody,
    idempotencyKey,
  }: {
    /**
     * App slug. Lowercase letters, digits, hyphens; must start and end with alnum.
     */
    slug: string,
    /**
     * Alert rule payload. See CreateAlertRuleRequest.
     */
    requestBody: CreateAlertRuleRequest,
    /**
     * Idempotency key for the POST. Stored for 24h. On replay the server
     * returns the original response with `Idempotent-Replayed: true`.
     *
     */
    idempotencyKey?: string,
  }): CancelablePromise<AlertRuleResponse> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/v1/apps/{slug}/alerts',
      path: {
        'slug': slug,
      },
      headers: {
        'Idempotency-Key': idempotencyKey,
      },
      body: requestBody,
      mediaType: 'application/json',
      errors: {
        400: `code: alert_rule_invalid | plan_alert_rules_not_allowed | plan_alert_rule_quota | image_egress_denied`,
        401: `code: unauthorized`,
        402: `code: alert_rule_invalid | plan_alert_rules_not_allowed | plan_alert_rule_quota | image_egress_denied`,
        403: `code: plan_limit_apps | plan_limit_ram | plan_limit_concurrency | plan_min_instances_not_allowed | plan_limit_secrets | plan_cron_quota | app_layer_too_large | image_egress_denied`,
        404: `code: not_found`,
        409: `code: alert_rule_invalid | plan_alert_rules_not_allowed | plan_alert_rule_quota | image_egress_denied`,
        429: `429. Two response shapes:
        - \`application/problem+json\` for code-driven 429s (\`plan_limit_concurrency\`, \`quota_exhausted\`).
        - \`text/plain\` for the authlimiter middleware (\`pkg/middleware/authlimit.go\`).
        `,
      },
    });
  }
  /**
   * Fetch one alert rule by id.
   * @returns AlertRuleResponse The alert rule.
   * @throws ApiError
   */
  public static getAlertRule({
    slug,
    id,
  }: {
    /**
     * App slug. Lowercase letters, digits, hyphens; must start and end with alnum.
     */
    slug: string,
    /**
     * 32-hex-char opaque ID (NOT canonical UUID).
     */
    id: string,
  }): CancelablePromise<AlertRuleResponse> {
    return __request(OpenAPI, {
      method: 'GET',
      url: '/v1/apps/{slug}/alerts/{id}',
      path: {
        'slug': slug,
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
   * Partial-update an alert rule.
   * Every field is optional. metric cannot cross families
   * (e.g. error_rate_pct → failed_invocations) — returns
   * 400 alert_rule_invalid.
   *
   * @returns AlertRuleResponse The updated alert rule.
   * @throws ApiError
   */
  public static updateAlertRule({
    slug,
    id,
    requestBody,
  }: {
    /**
     * App slug. Lowercase letters, digits, hyphens; must start and end with alnum.
     */
    slug: string,
    /**
     * 32-hex-char opaque ID (NOT canonical UUID).
     */
    id: string,
    requestBody: UpdateAlertRuleRequest,
  }): CancelablePromise<AlertRuleResponse> {
    return __request(OpenAPI, {
      method: 'PATCH',
      url: '/v1/apps/{slug}/alerts/{id}',
      path: {
        'slug': slug,
        'id': id,
      },
      body: requestBody,
      mediaType: 'application/json',
      errors: {
        400: `code: alert_rule_invalid | plan_alert_rules_not_allowed | plan_alert_rule_quota | image_egress_denied`,
        401: `code: unauthorized`,
        402: `code: alert_rule_invalid | plan_alert_rules_not_allowed | plan_alert_rule_quota | image_egress_denied`,
        404: `code: not_found`,
        429: `429. Two response shapes:
        - \`application/problem+json\` for code-driven 429s (\`plan_limit_concurrency\`, \`quota_exhausted\`).
        - \`text/plain\` for the authlimiter middleware (\`pkg/middleware/authlimit.go\`).
        `,
      },
    });
  }
  /**
   * Delete an alert rule.
   * @returns void
   * @throws ApiError
   */
  public static deleteAlertRule({
    slug,
    id,
  }: {
    /**
     * App slug. Lowercase letters, digits, hyphens; must start and end with alnum.
     */
    slug: string,
    /**
     * 32-hex-char opaque ID (NOT canonical UUID).
     */
    id: string,
  }): CancelablePromise<void> {
    return __request(OpenAPI, {
      method: 'DELETE',
      url: '/v1/apps/{slug}/alerts/{id}',
      path: {
        'slug': slug,
        'id': id,
      },
      errors: {
        401: `code: unauthorized`,
        402: `code: alert_rule_invalid | plan_alert_rules_not_allowed | plan_alert_rule_quota | image_egress_denied`,
        404: `code: not_found`,
        429: `429. Two response shapes:
        - \`application/problem+json\` for code-driven 429s (\`plan_limit_concurrency\`, \`quota_exhausted\`).
        - \`text/plain\` for the authlimiter middleware (\`pkg/middleware/authlimit.go\`).
        `,
      },
    });
  }
  /**
   * Mint a new webhook HMAC secret.
   * Server-mints a 32-byte secret, base64-encodes it, and
   * overwrites the row's sealed ciphertext in place. The
   * plaintext is NEVER returned in the response — the body
   * carries the masked constant + rotated_at only.
   *
   * @returns RotateAlertRuleSecretResponse Rotation succeeded.
   * @throws ApiError
   */
  public static rotateAlertRuleSecret({
    slug,
    id,
  }: {
    /**
     * App slug. Lowercase letters, digits, hyphens; must start and end with alnum.
     */
    slug: string,
    /**
     * 32-hex-char opaque ID (NOT canonical UUID).
     */
    id: string,
  }): CancelablePromise<RotateAlertRuleSecretResponse> {
    return __request(OpenAPI, {
      method: 'POST',
      url: '/v1/apps/{slug}/alerts/{id}/rotate-secret',
      path: {
        'slug': slug,
        'id': id,
      },
      errors: {
        401: `code: unauthorized`,
        402: `code: alert_rule_invalid | plan_alert_rules_not_allowed | plan_alert_rule_quota | image_egress_denied`,
        404: `code: not_found`,
        429: `429. Two response shapes:
        - \`application/problem+json\` for code-driven 429s (\`plan_limit_concurrency\`, \`quota_exhausted\`).
        - \`text/plain\` for the authlimiter middleware (\`pkg/middleware/authlimit.go\`).
        `,
      },
    });
  }
}

/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * Partial update — every field is optional. Nil/omitted means leave alone.
 */
export type UpdateAlertRuleRequest = {
  name?: string | null;
  enabled?: boolean | null;
  /**
   * Cannot cross metric families (e.g. error_rate_pct → failed_invocations) — returns 400.
   */
  metric?: 'error_rate_pct' | 'latency_p50_ms' | 'latency_p95_ms' | 'latency_p99_ms' | 'cold_start_pct' | 'request_count' | 'failed_invocations';
  comparison?: 'gt' | 'gte' | 'lt' | 'lte';
  threshold?: number | null;
  window_spec?: '5m' | '15m' | '1h' | '6h' | '24h' | '7d' | '15d';
  webhook_url?: string | null;
  /**
   * New plaintext HMAC secret. Omit to keep the existing secret.
   */
  webhook_secret?: string | null;
  cooldown_minutes?: number | null;
};


/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * A customer-configurable alert rule. Carries the masked webhook
 * secret; the sealed ciphertext is server-side only.
 *
 */
export type AlertRuleResponse = {
  id: string;
  /**
   * Pinned app id. Empty string = account-wide rule.
   */
  app_id: string;
  name: string;
  enabled: boolean;
  metric: 'error_rate_pct' | 'latency_p50_ms' | 'latency_p95_ms' | 'latency_p99_ms' | 'cold_start_pct' | 'request_count' | 'failed_invocations';
  comparison: 'gt' | 'gte' | 'lt' | 'lte';
  threshold: number;
  window_spec: '5m' | '15m' | '1h' | '6h' | '24h' | '7d' | '15d';
  /**
   * Source dimension for failed_invocations; null for every other metric.
   */
  failure_source?: 'any' | 'cron' | 'queue' | 'delayed_task' | 'async_invoke';
  webhook_url: string;
  /**
   * Literal "***" — the plaintext is never returned.
   */
  webhook_secret_sealed_masked: string;
  cooldown_minutes: number;
  /**
   * Cool-down state machine.
   */
  state: 'ok' | 'firing';
  last_fired_at?: string | null;
  last_evaluated_at?: string | null;
  created_at: string;
  updated_at: string;
};


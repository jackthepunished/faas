/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * Per-app metrics snapshot (issue #273 / ADR-041). Latencies are
 * milliseconds for the 2xx class only; failures surface as
 * `error_rate_pct`. `wake_p95_ms` is the FLEET p95 — the
 * underlying `gateway_wake_latency_seconds` histogram is
 * unlabeled. On Prometheus failure the endpoint returns 200 with
 * zeroed fields and `source: "degraded: <reason>"`.
 *
 */
export type AppMetricsResponse = {
  app_id: string;
  /**
   * Echoed window.
   */
  range: '5m' | '15m' | '1h' | '6h' | '24h' | '7d' | '15d';
  /**
   * "prometheus" on success, "degraded: <reason>" otherwise.
   */
  source: string;
  /**
   * RFC3339Nano UTC.
   */
  as_of: string;
  /**
   * Share of requests in the window. Drives the dashboard empty state.
   */
  request_count: number;
  /**
   * p50 of `gateway_request_duration_seconds_bucket{class="2xx"}` over the window, in ms.
   */
  latency_p50_ms: number;
  /**
   * p95 over 2xx traffic in the window, in ms.
   */
  latency_p95_ms: number;
  /**
   * p99 over 2xx traffic in the window, in ms.
   */
  latency_p99_ms: number;
  /**
   * Share of [45]xx requests in the window.
   */
  error_rate_pct: number;
  /**
   * Share of requests that triggered a cold boot (the WakeGate
   * leader). Followers waiting on the gate see zero cold
   * contribution; their wait is on the §12 dashboard via
   * `gateway_wake_queue_wait_seconds`.
   *
   */
  cold_start_pct: number;
  /**
   * FLEET p95 wake latency (the unlabeled histogram). Labelled as such in the UI.
   */
  wake_p95_ms: number;
};


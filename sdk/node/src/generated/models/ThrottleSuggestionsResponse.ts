/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
import type { ThrottleSuggestionRow } from './ThrottleSuggestionRow.js';
/**
 * Per-route throttle recommendation payload (ADR-091 D20.5
 * amendment, issue #881). Source is `prometheus` on success
 * or `degraded: <reason>` on Prometheus failure (response is
 * still 200 with empty Suggestions — the dashboard's
 * empty-state branch handles it).
 *
 */
export type ThrottleSuggestionsResponse = {
  app_id: string;
  range: string;
  /**
   * `prometheus` on success, `degraded: <reason>` on
   * PromQL failure.
   *
   */
  source: string;
  /**
   * RFC 3339 UTC timestamp the response was assembled.
   */
  as_of?: string;
  /**
   * True when `apps.route_metrics_enabled=false` (Free
   * plan). The response carries empty Suggestions plus
   * this flag so the dashboard can render the upsell
   * rather than a misleading zero.
   *
   */
  route_metrics_disabled: boolean;
  /**
   * Count of routes that collapsed into the reserved
   * `__route_other__` overflow bucket during the window
   * (ADR-093 cap = 50). A non-zero value indicates the
   * throttle is partial-coverage regardless of the
   * configured limit.
   *
   */
  routes_collapsed: number;
  /**
   * `plan.RateLimitRPS` — the sub-plan ceiling the
   * suggestion is clamped to. 0 on unknown plans (fail
   * OPEN — the apid sub-plan validator is the
   * authoritative gate).
   *
   */
  plan_ceiling_rps: number;
  /**
   * `plan.RateLimitBurst` — the sub-plan ceiling for
   * burst. 0 on unknown plans.
   *
   */
  plan_ceiling_burst: number;
  /**
   * The headroom factor the recommender applied to every
   * route (`Multiplier` constant). Echoed on the wire
   * so the strategy is auditable.
   *
   */
  multiplier: number;
  suggestions: Array<ThrottleSuggestionRow>;
};


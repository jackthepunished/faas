/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * Per-route token-bucket cap (ADR-091 D20.5 amendment,
 * issue #881). Customers tighten the per-route rps/burst
 * below their plan's `plan.RateLimitRPS` — the apid
 * validator enforces the sub-plan ceiling; the gateway
 * compiler enforces it again at load time.
 *
 * Sub-plan ceiling — the load-bearing constraint. A
 * throttle rule is STRICTLY a tightening primitive. A
 * rule that exceeds the ceiling is rejected with 422
 * BEFORE any DB write — a customer cannot raise their
 * plan limit by registering a throttle rule.
 *
 * Per-IP sub-keying is deliberately absent in v1 — see
 * the package doc on `pkg/state.EdgeRuleThrottleAction`
 * for the design rationale (memory-bounded limiter +
 * attacker-controlled IP cardinality = unbounded bucket
 * growth).
 *
 */
export type EdgeRuleThrottleAction = {
  /**
   * Token-bucket refill rate per route. The apid
   * validator rejects rps > plan.RateLimitRPS with a
   * 422. The gateway compiler clamps + warns on the
   * same bound at load time.
   *
   */
  requests_per_second: number;
  /**
   * Token-bucket burst per route. Mirrors rps: rejected
   * above `plan.RateLimitBurst` at create time and
   * clamped at compile time.
   *
   */
  burst: number;
};


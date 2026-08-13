/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * Per-route label snapshot (ADR-093). The bounded route label
 * set the gatewayd-internal control listener emits for the app.
 * Each item is `"<METHOD> <PATH>"` (pre-edge-rule-rewrite) for
 * an admitted route, or the reserved `"__route_other__"` overflow
 * bucket label. Bounded at 50 distinct real routes + the reserved
 * overflow per app (ADR-093 D2).
 *
 * `cap_hit` (ADR-093 Tier B item #1) is true iff the app's
 * route label set has reached `RouteMetricsPerAppCap` (50) and
 * additional routes are collapsing into the reserved
 * `__route_other__` overflow bucket. When `cap_hit` is true,
 * `len(routes) == RouteMetricsPerAppCap + 2` (50 real + the
 * reserved empty label + `__route_other__`). When false, the
 * dashboard can render "you have N admitted routes" without
 * having to count the array (which is ambiguous: 5 real routes
 * + `__route_other__` is indistinguishable from 50 real routes
 * + overflow). Omitted on the `source: unavailable` path —
 * the gatewayd-internal dial failed, the cap state is unknown.
 *
 */
export type AppRoutesResponse = {
  slug: string;
  app_id?: string;
  routes: Array<string>;
  source: 'live' | 'unavailable';
  /**
   * True iff the route label set has hit `RouteMetricsPerAppCap`
   * (50) and additional routes are collapsing into
   * `__route_other__`. Omitted on `source: unavailable` paths
   * (cap state is unknown when the gatewayd-internal dial
   * fails).
   *
   */
  cap_hit?: boolean;
};


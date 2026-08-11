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
 */
export type AppRoutesResponse = {
  slug: string;
  app_id?: string;
  routes: Array<string>;
  source: 'live' | 'unavailable';
};


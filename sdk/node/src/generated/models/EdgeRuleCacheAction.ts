/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
/**
 * Per-route TTL knobs for safe response caching on selected
 * GET/HEAD paths (ADR-122 / §4.1.2.17). The primitive for
 * "GET /catalog* → 60 s fresh + 5 min stale-on-error" without
 * forcing the customer to bring their own cache (Upstash
 * Redis). The hot-path applier
 * (pkg/gateway/handler_apply_edge_rule_cache.go) consults the
 * matched rule on each request; a hit serves the cached body
 * and returns BEFORE the wake gate, so no VM runs and no
 * `gb_ram_hour` accrues.
 *
 * Auth posture: requests carrying `Authorization` or a session
 * cookie are a hard bypass — they are NEVER stored and NEVER
 * served. `vary_on` therefore accepts ONLY
 * `Accept-Language` / `Accept-Encoding`; adding
 * `Authorization` / `Cookie` to `vary_on` is rejected at
 * create-time with 422.
 *
 * Field-by-field:
 * * `max_age_seconds` — fresh window in seconds. Default 60,
 * range `[0, 3600]`. A zero value disables fresh hits but
 * stale-on-error still applies within
 * `stale_if_error_seconds`.
 * * `stale_if_error_seconds` — post-fresh window during
 * which a stored entry may be served ONLY on origin
 * failure (wake gate failure or upstream 5xx/timeout).
 * Default 300, hard cap 300. Stale-while-revalidate
 * (serving stale while a refresh runs in the background)
 * is NOT supported.
 * * `vary_on` — closed vocabulary subset
 * (`Accept-Language`, `Accept-Encoding`) whose values
 * participate in the cache key. Empty = no vary
 * dimension beyond the URL.
 * * `methods` — optional method allowlist (default
 * `{GET, HEAD}`). Anything outside this set is rejected
 * at create-time with 422.
 *
 * Per-plan quota: Free 0 (closed), Hobby 1, Pro 5, Scale 20
 * (`Limits.EdgeRulesCachePerApp`). The Free=0 default is
 * deliberate — the wake-elision guarantee is the upsell, not
 * a baseline amenity.
 *
 */
export type EdgeRuleCacheAction = {
  /**
   * Fresh window in seconds (default 60, range [0, 3600]).
   */
  max_age_seconds: number;
  /**
   * Post-fresh window during which a stored entry may be served ONLY on origin failure (default 300, hard cap 300).
   */
  stale_if_error_seconds: number;
  /**
   * Closed vocabulary subset (Accept-Language, Accept-Encoding) whose values participate in the cache key.
   */
  vary_on?: Array<'Accept-Language' | 'Accept-Encoding'>;
  /**
   * Optional method allowlist (default {GET, HEAD}).
   */
  methods?: Array<'GET' | 'HEAD'>;
};


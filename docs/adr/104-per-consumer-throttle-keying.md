# ADR-104 · Per-consumer throttle keying (issue #881 Phase 3)

- **Status:** accepted
- **Date:** 2026-08-14
- **Decision:** Per-rule `kind=throttle` buckets key by an optional consumer
  dimension (`key_by ∈ {"none", "api_key", "jwt_subject", "jwt_claim"}`) chosen
  at rule-create time. The cardinality is bounded per-rule by
  `max_keys_per_rule` (Free 100 / Hobby 1000 / Pro 5000 / Scale 10000).
  When the per-rule consumer set exceeds the cap, all over-cap callers
  collapse into a single non-evicting `__other__` bucket that still
  consumes the parent rule's tokens.
- **Why:** PR #887 (ADR-091 D20.5 amendment) shipped `kind=throttle` keyed
  only by `appID + "\x00" + ruleID`. Customers asked for two follow-ups
  that the v1 bucket shape cannot express:
  1. **Per-API-key limits** — "this route, only key K-A1, gets 10 rps".
  2. **Per-JWT-bracket limits** — "this route, only JWTs with
     `tier=enterprise`, gets 100 rps".

  Without per-consumer keying, every authenticated caller of an app shares
  the same route bucket, so a noisy key can starve every other key. ADR-040
  ruled out per-API-key on the *wake path* (the gateway doesn't see
  `Authorization` there because `httputil.ReverseProxy` forwards it
  verbatim without inspection, see `docs/adr/040-per-account-rate-limit.md:31-42,196-202`).
  Phase 3 is the *edge-rule path* — a separate code path where the
  authn chain (`enforceRequireAuthn`, `applyEdgeRuleJWT`) already
  resolves the consumer identity. ADR-104 widens the throttle key
  construction to use that identity when a rule opts in.

  The bounded-design safety property (`__other__` collapse) is the
  load-bearing piece: without it, an attacker could mint many short-lived
  API keys (or rotate JWT subjects) to push the bucket map past its
  memory cap, then exploit the LRU eviction invariant of
  `pkg/gateway/ratelimit.go:234-267` to reset their own throttle.
  The `__other__` bucket is created exactly once per rule (lazy-init
  on first overflow) and is **pinned non-evictable** — it always
  consumes tokens regardless of how full it is, so an attacker who
  pushed past the cap still pays the parent rule's rps cost.
- **Consequences:**
  - New `EdgeRuleThrottleAction.KeyBy` (closed vocab, default `none`),
    `JWTClaimName` (regex `^[a-zA-Z_][a-zA-Z0-9_]{0,63}$`), and
    `MaxKeysPerRule` (Free 100 / Hobby 1000 / Pro 5000 / Scale 10000).
  - Wire-shape additive: existing rules keep `KeyBy=""` and behave
    exactly as PR #887 (today's behaviour preserved bit-for-bit).
  - New `Authenticated` struct on the request context, populated by
    `enforceRequireAuthn` (API key) and `applyEdgeRuleJWT` (subject +
    claims). Identity flows through to the throttle applier, which
    constructs the bucket key per `KeyBy`.
  - New `Limiter.AllowWithConsumerKey` (sibling of `AllowWithParams`)
    that owns the `__other__` collapse. Separate LRU scope
    `routeConsumerLimiter` (cap `EdgeRuleConsumerCacheCap = 100_000`,
    configurable) to keep the per-consumer bucket map bounded
    independent of the per-rule bucket map.
  - New CLI flags `--throttle-key-by`, `--throttle-jwt-claim`,
    `--throttle-max-keys-per-rule` on `cmd/gregale` create + update.
  - `make sdk-check` requires `KeyBy`/`JWTClaimName`/`MaxKeysPerRule`
    on every SDK (`sdk/go`, `sdk/node`, `sdk/python`).
  - PR-cluster outlined in
    `/Users/poyrazk/.claude/plans/serialized-greeting-fairy.md` (mega-PR
    bundling PR-0..3). Slot fence at
    `migrations/00267_reserve_slot.sql` (renumbered from 00266 after
    cross-PR precheck caught the ADR-101 OIDC cluster owning
    00265/00266).
  - Back-compat: pre-Phase-3 rules round-trip through the wire unchanged.
    The `kind=throttle` migration 00265 is the only DDL; Phase 3 is
    purely additive.
- **Rejected alternatives:**
  - **Single canonical identity (X-API-Key OR JWT, picked at platform
    level)** — rejected: customers have heterogeneous identity models
    (some are pure JWT, some are pure API keys, some are hybrid).
    Per-rule choice respects both shapes without forcing one onto
    the other.
  - **Unbounded per-consumer keying** — rejected: the existing LRU
    limiter has no per-bucket TTL, so unbounded cardinality is a
    memory-exhaustion vector. The `__other__` collapse is the
    answer; unbounded growth is not.
  - **Per-IP instead of per-consumer** — rejected: per-IP multiplies
    cardinality by unique-IP count (unbounded, attacker-controlled),
    and is already explicitly out-of-scope per ADR-091 D20.5
    amendment ("Per-IP sub-keying is deliberately out of v1").
    Phase 3 doesn't reopen this decision.
  - **Central mode (`pg_ratelimit_counters`)** — rejected: that table
    has zero Go readers today (per PR #887 §"Central mode is
    schema-only" caveat). The cross-replica drift remains a known,
    documented limitation. Phase 3 inherits it.
  - **Server-side consumer allowlist** — rejected for Phase 3: any
    authenticated key is treated as a valid consumer. Operator-curated
    allowlists are a Phase 4 follow-up.
  - **Phase 3 on the wake path** — rejected: ADR-040 §196-202
    explicitly closed this. Phase 3 is the edge-rule path only.
    Wake-path per-account rate limiting continues to use
    `app.AccountID` (ADR-040 §31-42).

## Implementation seams

- Bucket key construction:
  - `none` → `appID + "\x00" + ruleID` (PR #887 shape)
  - `api_key` → `appID + "\x00" + ruleID + "\x00" + apiKeyID`
  - `jwt_subject` → `appID + "\x00" + ruleID + "\x00" + jwtSubject`
  - `jwt_claim` → `appID + "\x00" + ruleID + "\x00" + claimValue`
  - over-cap → `appID + "\x00" + ruleID + "\x00" + "__other__"` (single
    bucket per rule, pinned, never evictable)
- Identity sources:
  - `enforceRequireAuthn` (`pkg/gateway/handler.go:962-1031`) — stamps
    `Authenticated.APIKeyID` on the success path; today the value is
    dropped at line 988. Phase 3 keeps it via `withAuthenticated(ctx, …)`.
  - `applyEdgeRuleJWT` (`pkg/gateway/handler.go:1643-1700`) — stamps
    `Authenticated.JWTSubject` + `Authenticated.JWTClaims` after JWKS
    verification, sourced from `pkg/edgejwks/verifier.go:46-52`.
- LRU interaction:
  - `routeLimiter` (today, `NewLimiterWithLRU(EdgeRuleCacheCap=10_000)`)
    continues to hold the per-rule bucket. Phase 3 keeps the
    `appID + "\x00" + ruleID` shape here unchanged.
  - `routeConsumerLimiter` (NEW, `NewLimiterWithLRU(EdgeRuleConsumerCacheCap=100_000)`)
    holds the per-rule per-consumer buckets plus the `__other__` overflow.
    `__other__` is marked non-evictable in `Limiter` bookkeeping
    (mirrors the full-bucket-only invariant at `pkg/gateway/ratelimit.go:234-267`).
- Property tests (the load-bearing ones):
  - `TestRouteConsumerThrottle_OtherBucketPinnedEvenWhenFull` —
    the `__other__` bucket must NOT be evictable even at full.
  - `TestRouteConsumerThrottle_OverCapCollapse` — 10,001 distinct keys
    against one rule with `MaxKeysPerRule=1000` produce 1000 consumer
    buckets + 1 `__other__` and no growth beyond that.
  - `TestRouteConsumerThrottle_NoBackCompatRegression` — pre-Phase-3
    rules round-trip through the wire and behave identically to PR #887.

## Wire + state + schema

- `pkg/api/dto.go::EdgeRuleThrottleAction` — adds `{KeyBy, JWTClaimName, MaxKeysPerRule}`
  + `Validate()` enforcing closed vocab + regex on `JWTClaimName` + range on
  `MaxKeysPerRule`.
- `pkg/state/types.go::EdgeRuleThrottleAction` — mirrors the three fields.
- `pkg/api/limits.go::ThrottleMaxKeysPerRule` — Free 100 / Hobby 1000 /
  Pro 5000 / Scale 10000.
- `api/openapi.yaml` + `pkg/apid/openapi.yaml::EdgeRuleThrottleAction`
  schemas add the three properties; `make spec-sync` regenerates the
  embedded copy.
- No new CHECK constraints on `edge_rules.action` jsonb — it is
  free-form today; ADR-091 D20.5 deliberately kept it that way.

## Out of scope (deferred to Phase 4 or new ADR)

- `jwt_claim: <name>` for non-string claims (numbers, arrays, objects)
- Per-consumer rate-limit *quotas* (max number of per-consumer rules an
  account can configure) — uses existing `Limits.EdgeRulesThrottlePerApp`
  for now.
- Server-side allowlist of consumer IDs.
- Per-IP variant (explicit ADR-091 D20.5 deferral preserved).
- Per-country / per-user-ID limits.
- Auto-applying recommendations.

## References

- ADR-040 (per-account rate limit, wake-path policy)
- ADR-091 D20.5 amendment (kind=throttle ship — PR #887)
- ADR-093 (cap+overflow precedent for `kind=limit` collapsed routes)
- ADR-041 (migration slot reservation pattern; slot 00267 fence)
- `pkg/gateway/ratelimit.go:143-172` (`AllowWithParams` signature)
- `pkg/gateway/ratelimit.go:234-267` (full-bucket-only LRU invariant)
- `pkg/gateway/handler.go:962-1031` (`enforceRequireAuthn`)
- `pkg/gateway/handler.go:1643-1700` (`applyEdgeRuleJWT`)
- `pkg/edgejwks/verifier.go:46-52` (`Claims.Custom` map)
- `pkg/state/types.go:294-313` (`APIKey.ID` field)
- Plan file: `/Users/poyrazk/.claude/plans/serialized-greeting-fairy.md`
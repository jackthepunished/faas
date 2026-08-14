-- filename: 00244_edge_rules_kind_throttle.sql
-- +goose Up
-- +goose StatementBegin

-- Edge rule kind=throttle (per-route per-method rate limiting,
-- ADR-091 D20.5 amendment; issue #881 PR-A). Distinct from
-- kind=limit (which is a body-size cap, 413, PR-845). Distinct
-- from the plan-tier shared limiters at
-- pkg/gateway/ratelimit.go:57 (per-app) and :75 (per-account),
-- which are platform-controlled and plan-keyed; here the customer
-- chooses a non-null RPS+burst for one (host, path, http_method)
-- triple, and a 429 response with Retry-After: 1 +
-- x-faas-rate-limit-scope: route + X-RouteRateLimit-{Limit,
-- Remaining,Reset} is emitted when the rule fires.
--
-- The action blob carries `requests_per_second` (float, ≥1) and
-- `burst` (int, ≥1). Both are bounded above by the customer's
-- plan ceiling (F1/H5/P25/S100 — see EdgeRulesThrottlePerApp in
-- pkg/api/limits.go), with the request budget additionally
-- constrained to the platform constants
-- api.RateLimitRPS (= 100, plan-tier) and api.RateLimitBurst.
-- Customers tighten, never raise: validateEdgeRuleAction in
-- cmd/apid/handlers_edge_rules.go rejects rps > plan.RateLimitRPS
-- with a 422 carrying limit + observed + docs URL, mirroring
-- the kind=limit sub-plan ceiling pattern.
--
-- Cardinality is bounded by *configured rules*, not by traffic,
-- because the bucket key is `appID + "\x00" + ruleID`. Per-IP
-- sub-keying is deliberately excluded from v1 — that multiplies
-- cardinality by unique-IP count, which is unbounded and
-- attacker-controlled, against a limiter that has never had
-- eviction. Shipping the field now and bounding it later is not
-- safe; an ADR-093-style cap+__ip_other__ overflow is the
-- prerequisite.
--
-- Eviction safety is the load-bearing PR-B invariant (PR #887):
-- a partially-drained bucket must never be evicted, otherwise an
-- attacker can force eviction to reset their own bucket and the
-- limit becomes a no-op. PR-B guarantees eviction only for full
-- buckets (tokens >= burst) which carry no state.
--
-- The new kind is open to Free + every other plan (no plan gate;
-- mirrors kind=validate / kind=limit / kind=maintenance
-- posture). Per-kind quota EdgeRulesThrottlePerApp (Free=1,
-- Hobby=5, Pro=25, Scale=100, enforced in
-- pkg/state/pgstore.go::CreateEdgeRuleIfUnderQuota) caps the
-- cardinality per app rather than gating by plan tier.
--
-- DROP+ADD pair is canonical (migrations/00214, 00219, 00229,
-- 00236) because Postgres 15 (CI) does not accept `ADD CONSTRAINT
-- IF NOT EXISTS`. The constraint name `edge_rules_kind_check`
-- is the Postgres-assigned default for an inline CHECK on `kind`.
--
-- Ordering hazard vs PR #845 (kind=geo, ADR-091 D21-D23): the
-- 00229 widens to include 'geo'; PR-A's 00236 widens to include
-- 'maintenance'. The CHECK list below is the union of all
-- shipped kinds up to and including kind=throttle (12 values).
-- The list MUST include 'geo' and 'maintenance' (they run BEFORE
-- this migration); otherwise the DROP+ADD pair would silently
-- drop them from the closed vocabulary.
--
-- PR #884 (ADR-099 tenant-surfaces PR-0) reserves 00238-00243 on
-- its open branch; this migration takes the next slot 00244 so
-- the merge order is unambiguous: PR #884 lands first (its fences
-- do not depend on schema), then this one re-merges on top.
-- After rebase the kind=throttle file stays at 00244 because none
-- of 00238-00243 contain real DDL — they're pure fences.
--
-- PR #864 (kind=budget, slot 00231) widens to include 'budget'
-- but a separate comment block in 00236 notes PR #864's CHECK
-- omitted 'geo'; 00236 fixed that. If 00231 lands BEFORE this
-- migration, the post-apply CHECK already includes 'budget' (and
-- 'geo' thanks to 00236); the union-widening this migration
-- applies is {prior 11} + throttle = 12, identical final shape
-- whether 00231 has merged or not.

ALTER TABLE edge_rules DROP CONSTRAINT IF EXISTS edge_rules_kind_check;
ALTER TABLE edge_rules ADD CONSTRAINT edge_rules_kind_check
  CHECK (kind IN ('route', 'rewrite', 'redirect', 'headers',
                  'cors', 'jwt', 'ip', 'validate', 'limit', 'geo',
                  'maintenance', 'throttle'));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Reverse the widening. Throttle-kind rules created between
-- this migration's apply and a downgrade become the violators
-- and force the downgrade to fail with 23514 — same safety
-- contract as 00214 / 00219 / 00229 / 00236's reverses. The
-- reverse CHECK must still include 'geo' and 'maintenance'
-- (they were added by earlier migrations at 00229 / 00236 and
-- must not be silently dropped here).
ALTER TABLE edge_rules DROP CONSTRAINT IF EXISTS edge_rules_kind_check;
ALTER TABLE edge_rules ADD CONSTRAINT edge_rules_kind_check
  CHECK (kind IN ('route', 'rewrite', 'redirect', 'headers',
                  'cors', 'jwt', 'ip', 'validate', 'limit', 'geo',
                  'maintenance'));

-- +goose StatementEnd

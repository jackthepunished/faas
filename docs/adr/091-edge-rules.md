# ADR-091 · Edge rules (issue #561 / routes#rev r=2)

- **Status:** accepted (PR-cluster landing PR-1 through PR-5)
- **Date:** 2026-08-10
- **Issue:** #561 (customer-facing per-request routing/decisions: kind=route / rewrite / redirect / headers / cors / jwt / ip)
- **Related:** spec §13.4 (gateway hot-path ordering), ADR-070 (Tier A7 edge split — the architecture that places `gatewayd-public` and `gatewayd-internal` on opposite sides of a unix socket), ADR-009 (the identical inner network world that lets PR 3's `kind=route` substitute freely), ADR-001–010 (inlined in spec §3), §11 (egress / host-age / cgroups posture that bounds the JWKS URL validator), ADR-041 (slot-reservation pattern used by the matching migration).
- **Supersedes:** the per-deployment "routing rules" feature deferred from issue #561 / Q3'26.

## Context

Customers want per-host request shaping beyond what deployments expose today.
A typical request is "host `foo.example.com` → rewrite `/v1/*` → `/api/*`, allow
CORS from `https://app.example.com`, require a Bearer JWT signed by our IdP, and
block any client outside `192.0.2.0/24` from `/admin/*`". The deployments row
today only carries run-time concerns (image, env, RAM, concurrency) — a separate
resource is needed for the **per-host decision set**. Issue #561 frames this as
"edge rules", borrowing Cloudflare's Ruleset Engine vocabulary: a ruleset has 7
kinds of rules, each rule attaches to (host, path, methods), and ordering
between rules of different kinds matches the engine.

The seven kinds are intentionally narrow:

| kind        | Effect                                                            | Free?  |
|-------------|-------------------------------------------------------------------|--------|
| `route`     | Substitute the inbound host with another deployment's app         | yes    |
| `rewrite`   | Mutate `r.URL.Path` (single-shot prefix rewrite)                  | yes    |
| `redirect`  | 3xx the inbound request to a different URL                         | yes    |
| `headers`   | Stamp request/response headers (allowlist; bans Host/Content-* / `x-faas-*`) | yes |
| `cors`      | CORS preflight short-circuit + response-header stamper             | yes    |
| `jwt`       | Inbound Bearer JWT verifier (per-rule JWKS, iss/aud/alg/claims)   | paid   |
| `ip`        | Allow/deny CIDR list evaluation against trusted XFF                | paid   |

PRs 1-4 of the rollout (#799 schema, #815 CLI, #820 kind=route matcher, #824
kind=rewrite/redirect/headers matcher) all cite "ADR-089 / issue #561" in
code comments. `grep -i 'edge rule' docs/adr/` returns nothing — ADR-089 is
occupied by build-status-endpoint / secret-rotation / standby-write-redirect /
pr-cluster-outline, none of which are about edge rules. **This ADR closes the
cite-bug in one shot** by replacing every "ADR-089 / edge rules" reference with
"ADR-091 / edge rules". (The other ADR-089 siblings — including secret-rotation
PR-A/B/C — keep their cite numbers untouched.)

## §3 Sub-decisions

1. **Seven kinds, three plan tiers.** CORS is Free. JWT and IP are Hobby+.
   `kind=route|rewrite|redirect|headers` are Free and unconstrained. Plan gating
   fires at create-time in `cmd/apid/handlers_edge_rules.go:184-201` via
   `state.EdgeRuleKind.IsPaidOnly()`; the 402 problem includes a stable `code`
   (`plan_edge_rule_kind_not_allowed`) and a docs URL.

2. **Each rule carries its own (host, path, methods) match filter, priority, and
   same-account binding.** Path-globs validate via `path.Match` at apid
   create-time AND at gateway compile time (PR 3 R3 fix — never trust the SQL
   hotfix path). Priorities sort ASC; per-kind matchers return the
   lowest-numbered match. Cross-account rules at the gateway log `edge_rule
   .{kind}_blocked` audit + `outcome=blocked` metric and fall through silently
   (PR 4 pattern).

3. **`pg_notify` invalidation is wholesale** (`pkg/gateway/EdgeRuleCache.Reset()`).
   No new pg_notify channel for PR 5. Per-host LRU mirrors `RouteCache`. The
   cache is `Reset()`-on-`db.NotifyEdgeRuleChanged` (single ops surface — every
   update funnels through one notify; gatewayd-internal subscribes; PR 6 will
   add the property test that pins the wholesale invariant).

4. **Hot-path ordering in `ServeHTTP`** (spec §13.4 + Cloudflare Ruleset
   Engine):

   ```
   matchAndSubstituteRoute          # PR 3: kind=route
     → applyEdgeRuleCORS            # PR 5: kind=cors (preflight 204 before redirect)
     → matchAndApplyRedirect        # PR 4: kind=redirect (3xx short-circuit)
     → matchAndApplyRewrite         # PR 4: kind=rewrite (mutates r.URL.Path)
     → applyEdgeRuleHeaders         # PR 4: kind=headers (response header ops)
     → applyEdgeRuleJWT             # PR 5: kind=jwt  (post-route, pre-wake-gate)
     → applyEdgeRuleIP              # PR 5: kind=ip   (cheap deny before auth)
     → enforceRequireAuthn
     → enforcePublicAuth
     → Backend.Lookup
     → ...
   ```

   CORS precedes redirect so an OPTIONS preflight returns 204 (browsers reject
   preflights that 3xx). JWT and IP precede per-deployment authority gates so a
   JWT-failed request never reaches `require_authn=true`. CORS, JWT, and IP all
   precede `Backend.Lookup` so a rejected request never pays a cold-wake cost.

5. **Same-account enforcement is the only security boundary at the gateway.**
   Cross-account rules fall through silently. Apid create-time rejects
   same-account violations up front (existing `App.AccountID` check);
   `cmd/gatewayd-internal/edge_rules.go`'s `Match*` methods add a runtime
   same-account assertion as defense-in-depth even if the create-time check
   were bypassed.

6. **Metrics** — single counter widens: `gateway_edge_rule_match_total
   {kind, outcome}`. The pre-instantiation loop (`pkg/gateway/metrics.go
   :528-535`) initialises the cross-product `{kind ∈ route|rewrite|redirect|
   headers|cors|ip|jwt} × {outcome ∈ match|miss|blocked|failed|missing}` from
   the first scrape so dashboards see all labels without a "first-scrape gap".
   PR 5 widens the outcome vocab to 5 (added `failed`, `missing`) but the
   `{match,miss,blocked}` triple continues to work for the 4 prior kinds.
   No new `CounterVec`.

7. **Audit emissions** — `edge_rule.<kind>_matched` / `edge_rule.<kind>_blocked`
   for the 4 prior kinds. PR 5 widens: `edge_rule.jwt_matched|blocked|failed
   |missing` (4 kinds, since JWT has more failure modes than the others);
   `edge_rule.cors_matched|blocked`; `edge_rule.ip_matched|blocked`. Audits
   are emitted through the existing `gatewaydEdgeRulesAud.Emit` thin wrapper
   (`cmd/gatewayd-internal/edge_rules.go:440-460`).

8. **CORS preflight short-circuit** in `applyEdgeRuleCORS`: OPTIONS + a
   matching `kind=cors` rule → 204 with `Access-Control-Allow-*` headers,
   no `Backend.Lookup`. Mirrors `http.Redirect`'s pattern (write header,
   never `Write()`). Non-preflight stamps `Access-Control-Allow-Origin`
   via the `statusRecorder` hook (`pkg/gateway/handler.go`, response-side
   header-mutation pattern). PR 5 ships `applyEdgeRuleCORS` as the canonical
   reference shape.

9. **JWT verification** — `pkg/edgejwks` (NEW) wraps `go-jose/v4/jwk.NewCache`
   for the JWKS fetch+rotation work and exposes a narrow `Cache`/`Verifier`
   interface so cmd-side code never imports `go-jose` directly. Clock
   skew is `60s` (matches Auth0/Okta/Cognito defaults). Verifier returns
   typed errors (`ErrJWTMissingToken`, `ErrJWTExpired`, `ErrJWTBadSignature`,
   `ErrJWTWrongIssuer`, `ErrJWTWrongAudience`, `ErrJWTMissingClaim`,
   `ErrJWKSNotRegistered`) so `applyEdgeRuleJWT` can pick the right audit
   kind (`jwt_missing` vs `jwt_failed`).

10. **JWKS URL defense-in-depth at apid-Validate** — `EdgeRuleJWTAction
    .Validate()` (PR 5) requires `JWKSURL` start with `https://` AND rejects
    the RFC1918 / loopback / link-local prefix list (`https://localhost`,
    `https://127.*`, `https://10.*`, `https://192.168.*`, `https://169.254.*`,
    `https://[::1]`, `https://[fc*::*`, `https://[fd*::*`). Cheap string-prefix
    check (defense-in-depth; a future ADR can upgrade to `net.ParseIP` +
    `IsPrivate`/`IsLoopback`/`IsLinkLocalUnicast` for IPv6-multicast edges).
    §11 egress posture already forbids RFC1918 / link-local / metadata ranges
    at the host firewall — this is the application-layer equivalent.

11. **HS\* dropped from JWT algorithm vocabulary** — PR 1's `pkg/api/dto.go
    ::edgeRuleJWTAllowedAlgs` originally listed `RS256/384/512 + ES256/384/512
    + HS256/384/512`. PR 5 shrinks the vocabulary to the 6 asymmetric
    algorithms only. Rationale: HS\* over JWKS means a symmetric key served
    from a public endpoint — anyone with the URL can forge tokens. The action
    shape has no `secret_ref` field (only `jwks_url`), so HS\* was structurally
    broken. If customers later need HS\* with a shared secret, that's a new
    `secret_ref` action shape and a new ADR. CLI vocab (`cmd/gregale/commands
    _edge_rules.go::edgeRuleJWTAlgVocab`) shrinks in lockstep. Validator 422s
    `HS*` immediately rather than failing at runtime.

12. **CORS `*`+credentials footgun guarded at apid-Validate** — `EdgeRule
    CORSAction.Validate()` (PR 5) returns 422 if `AllowOrigins: ["*"]`
    combined with `AllowCredentials: true`. Browsers reject this combination
    (`Access-Control-Allow-Origin: *` cannot pair with `Allow-Credentials:
    true`). Cheaper to reject at create-time than to ship a rule that
    silently fails in production. The gateway stamper trusts validated input.

13. **Client-IP provenance for `kind=ip`: X-Forwarded-For single trusted hop.**
    The Tier A7 architecture puts `gatewayd-public` and `gatewayd-internal`
    on opposite sides of a unix socket (`pkg/gateway/internal_proxy.go:84-91`,
    `cmd/gatewayd-public/main.go:1-43`). When a request crosses the unix
    socket, `r.RemoteAddr` in `gatewayd-internal` is the unix-socket peer,
    **not** the customer's IP. The customer IP is forwarded via the single
    trusted `X-Forwarded-For` entry that `pkg/gateway/internal_proxy.go
    :286-289` writes (`internal_proxy_test.go:149` pins `len == 1`).
    `applyEdgeRuleIP` reads that one entry via the helper `clientIPFrom
    TrustedXFF(r *http.Request) (net.IP, bool)`. Defense-in-depth:
    `len(r.Header.Values("X-Forwarded-For")) != 1` falls through with
    `edge_rule.caller_ip_forged` audit (deny-by-default posture). The
    gateway-side assertion is the load-bearing one — even if a misbehaving
    caller appends to inbound XFF, `internal_proxy.go:286` strips the chain
    before re-adding the public daemon's `RemoteAddr`, so the gateway
    only ever sees the trusted 1-hop value.

14. **JWKS cache TTL** — `jwk.NewCache` with `WithMinRefreshInterval(5 *
    time.Minute)` and the background refresh every **15 minutes** (matching
    Cloudflare Access). Memory cap = **1024 keys per URL**; on overflow the
    cache evicts and re-fetches. The `pkg/edgejwks.Cache` interface is
    narrow (`Get | Register | Reset`) so cmd-side code never imports
    `go-jose` directly.

15. **Per-rule JWKS URL, not account-level** — each `kind=jwt` rule stores
    its own `jwks_url`. Account-level JWKS deferred to a future ADR if
    customers ask. Per-rule keeps tenancy boundaries obvious and avoids
    creating a separate "jwks_endpoints" table at PR-time.

16. **No new migrations.** PR 1 already added the `edge_rules` table
    (`migrations/00192_edge_rules.sql`) with all 7 kinds in the schema
    CHECK constraint. PRs 2-5 are non-schema. PR 6 will add the LRU
    property test that pins the wholesale `Reset()` invariant.

## Implementation surface

| Layer | File | Change |
|-------|------|--------|
| ADR | `docs/adr/091-edge-rules.md` | This file. NEW. |
| Cite-bug cleanup | `*.go` + `*.md` (all `*, "ADR-089"` / `, ADR-089` referring to edge rules) | REPLACE → "ADR-091". (Other ADR-089 siblings — secret-rotation etc. — keep their numbers untouched.) |
| Migration | — | None. (`00192_edge_rules.sql` already exists with all 7 kinds in the CHECK.) |
| DTO | `pkg/api/dto.go::EdgeRuleJWTAction.Validate` | ADD: reject `HS*` algs; reject RFC1918/loopback/link-local JWKS URL prefixes. |
| DTO | `pkg/api/dto.go::EdgeRuleCORSAction.Validate` | ADD: reject `AllowOrigins:["*"]` + `AllowCredentials:true`. |
| Limits | `pkg/api/limits.go` | None (CORS Free; JWT/IP already in `LimitsFor(...).EdgeRules{JWT,IP}Allowed`). |
| State types | `pkg/state/types.go` | None (PR 1 shipped all 7 kinds + actions). |
| State pgstore | `pkg/state/pgstore.go` | None (same SQL covers all 7 kinds). |
| New package | `pkg/edgejwks/cache.go` | NEW. LRU-wrapped `jwk.NewCache`. `Get | Register | Reset`. |
| New package | `pkg/edgejwks/verifier.go` | NEW. `Verify(ctx, rawToken, rule) (*Claims, error)` with 60s skew. |
| Gateway | `pkg/gateway/edge_rules.go` | ADD: `EdgeRule{CORS,JWT,IP}Resolved` types; `HostEntry` widens to 7 slices; `EdgeRuleMatcher` widens with `Match{CORS,JWT,IP}`; 3 `PickFirst*Match` helpers; `EdgeRuleCache.Get{CORS,JWT,IP}`; `noOpEdgeRuleMatcher` widens. |
| Gateway | `pkg/gateway/handler.go::applyEdgeRule{CORS,JWT,IP}` | NEW. ≤50 lines each (extracted helpers, matching PR 4 discipline). |
| Gateway | `pkg/gateway/handler.go::ServeHTTP` injection sites | CORS between route-substitute and redirect; JWT/IP after headers before `enforceRequireAuthn`. |
| Gateway | `pkg/gateway/handler.go::clientIPFromTrustedXFF` | NEW. Helper for `kind=ip`. |
| Gateway | `pkg/gateway/metrics.go::ObserveEdgeRuleMatch` | Pre-instantiation widens to 7 kinds × 5 outcomes. |
| Gateway | `pkg/gateway/internal_proxy.go` | None — already writes the trusted single-hop XFF (PR #749). |
| Config | `cmd/gatewayd-public/main.go` | None (RemoteAddr not rewritten; XFF is the source). |
| Compile | `cmd/gatewayd-internal/edge_rules.go::compile{CORS,JWT,IP}Rules` | NEW. Mirror of `compile{Route,Rewrite,Redirect,Headers}Rules`. |
| Compile | `cmd/gatewayd-internal/edge_rules.go::loadHost` | WIDEN to recompile all 7 kinds in one SQL pass. |
| Match | `cmd/gatewayd-internal/edge_rules.go::Match{CORS,JWT,IP}` | NEW. Cache.Get → store miss → compile → Put → PickFirst. |
| Wrapper | `cmd/gatewayd-internal/edge_rules_jwks.go` | NEW. Constructs the real `pkg/edgejwks.Cache` from `jwk.NewCache`. |
| Wire | `cmd/gatewayd-internal/run.go::run` | WIDEN to pass `edgejwks.Cache` into `gatewaydEdgeRules`. |
| Compile-time guard | `cmd/gatewayd-internal/edge_rules.go` | `var _ gateway.EdgeRuleMatcher = (*gatewaydEdgeRules)(nil)` (already exists at `:471`). |
| CLI | `cmd/gregale/commands_edge_rules.go::edgeRuleJWTAlgVocab` | SHRINK to RS/ES 256/384/512 (lockstep with `pkg/api/dto.go::edgeRuleJWTAllowedAlgs`). |
| Tests | `pkg/gateway/edge_rules_test.go` | +200 LOC. PickFirst tests, HostEntry round-trip, interface widening. |
| Tests | `pkg/gateway/handler_test.go` | +350 LOC. CORS preflight/non-preflight, JWT passthrough/failure, IP allow/deny/wins, ordering, malformed-CIDR. |
| Tests | `pkg/gateway/metrics_test.go` | +20 LOC. Pre-instantiation label coverage. |
| Tests | `pkg/edgejwks/cache_test.go` | +100 LOC. Get/Put/LRU-evict/Reset/concurrent. |
| Tests | `pkg/edgejwks/verifier_test.go` | +150 LOC. Valid/expired/wrong-iss/wrong-aud/wrong-alg/required-claims/bad-sig/clock-skew/rotation. |
| Tests | `cmd/gatewayd-internal/edge_rules_test.go` | +250 LOC. compile* + Match* (cache-miss-hits-store) + shared-cache. |
| Tests | `cmd/apid/handlers_edge_rules_test.go` | +60 LOC. HS\* reject, CORS `*`+cred reject, JWKS localhost/private reject, RS256 still works. |
| Tests | `cmd/sdk-coverage/main.go` | Verify auto-derivation still covers 7 kinds (no change required). |
| `go.mod` | — | PROMOTE `github.com/go-jose/go-jose/v4` from indirect to direct. |

## Verification

### Local

```bash
go test -race ./pkg/gateway/... ./pkg/edgejwks/... ./cmd/gatewayd-internal/... ./cmd/apid/... -count=1 -timeout 5m

# Lint
make lint
go fmt -l . | wc -l    # 0

# Full repo
go test ./... -count=1 -timeout 10m

# Cite-bug sweep gate
grep -rEn 'ADR-089' --include='*.go' --include='*.md' . \
  | grep -v 'docs/adr/089-' \
  | grep -vE 'commands_(builds|build_status|secrets_rotate|2)\.(go)' \
  | grep -vE 'server\.go|cmd/gregale/commands2' \
  | grep -vE 'docs/runbooks/standby-write-redirect\.md' \
  | grep -vE 'tests/property/write_redirect_test\.go' \
  | grep -vE 'package rekey|pkg/rekey|pkg/secretbox/kid\.go|pkg/api/secrets\.go' \
  | grep -vE 'cmd/apid/handlers_secrets_rotate(_test)?\.go' \
  | grep -vE 'migrations/00191_app_secrets_kid_test\.go' \
  | grep -vE 'pkg/api/(errors|client_method_sweep_test|client|limits(_test)?)\.go' \
  | grep -vE 'cmd/sdk-coverage/main\.go' \
  | grep -vE 'cmd/e2e/standby_write_redirect_e2e_test\.go' \
  | grep -vE 'pkg/state/(memstore|pgstore_edge_rules_test|pgstore|store)\.go' \
  | wc -l    # expect: ≤18 (the non-edge-rules siblings that legitimately cite ADR-089 secret-rotation etc.)
```

The cite-bug sweep is intentionally surgical — a blanket `s/ADR-089/ADR-091/g`
would mis-cite the secret-rotation / standby-write-redirect / build-status-endpoint
ADRs. Only edge-rules references flip.

### Manual smoke against a real `gatewayd-internal`

```bash
# 1. kind=cors preflight
FAAS_API=http://localhost:8080 FAAS_TOKEN=fp_live_xxx gregale edge-rules create \
  --app demo --kind cors \
  --match-host foo.example.com \
  --cors-allow-origin https://app.example.com \
  --cors-allow-method GET --cors-allow-method POST \
  --cors-allow-header Authorization \
  --cors-allow-header Content-Type
curl -i -X OPTIONS \
  -H "Origin: https://app.example.com" \
  -H "Access-Control-Request-Method: POST" \
  http://localhost:8080/api/foo
# → 204 + Access-Control-Allow-* headers (preflight short-circuit)

# 2. kind=ip deny
FAAS_API=http://localhost:8080 FAAS_TOKEN=fp_live_xxx gregale edge-rules create \
  --app demo --kind ip \
  --match-host foo.example.com \
  --ip-deny 192.0.2.0/24
curl -H "X-Forwarded-For: 192.0.2.42" \
  http://localhost:8080/api/foo
# → 403 (server-side IP deny fires via the trusted single-hop XFF)

# 3. kind=jwt
FAAS_API=http://localhost:8080 FAAS_TOKEN=fp_live_xxx gregale edge-rules create \
  --app demo --kind jwt \
  --match-host foo.example.com \
  --jwt-issuer https://idp.example.com/ \
  --jwt-jwks-url https://idp.example.com/.well-known/jwks.json \
  --jwt-algorithm RS256 \
  --jwt-audience https://api.example.com
curl -i -H "Authorization: Bearer $TOKEN" \
  http://localhost:8080/api/foo
# → 200 (if token verifies)
curl -i http://localhost:8080/api/foo
# → 401 (no token → edge_rule.jwt_missing)

# Metric surfaces from boot (no first-scrape gap)
curl localhost:9090/metrics | grep gateway_edge_rule_match_total
# expect labels:
#   kind=route|rewrite|redirect|headers|cors|ip|jwt
#   × outcome=match|miss|blocked (+ failed,missing for jwt)

# Live invalidation (wholesale — same as PR 3/4)
psql -c "select pg_notify('edge_rule_changed', '{\"op\":\"updated\"}')"
```

### CI

```bash
# 24 checks (parallel) — all green
gh pr checks <PR-NUMBER> --watch
```

### Pre-merge checklist

- [ ] `gatewayd-public` populates `r.RemoteAddr` with the **client's** IP
  (Caddy → public → internal), and `pkg/gateway/internal_proxy.go:286-288`
  re-adds that as the single trusted XFF entry. (`internal_proxy_test.go:149`
  pins `len == 1`.)
- [ ] JWKS URL network-position validation at apid-Validate (D10).
- [ ] HS\* algorithms dropped from BOTH `pkg/api/dto.go::edgeRuleJWTAllowedAlgs`
  AND `cmd/gregale/commands_edge_rules.go::edgeRuleJWTAlgVocab` (D11). Both
  vocabs must match exactly.
- [ ] CORS `*`+credentials footgun guarded at apid-Validate (D12).
- [ ] `grep -nE 'ADR-089' --include='*.go' --include='*.md'` returns ≤18 hits
  post-cite-bug sweep (legitimate secret-rotation siblings stay).
- [ ] PR 4 review-fix lessons applied — every `h.observe(...)` passes
  `string(app.Plan)`; every `r.URL.Path` concat handles the leading-`/` edge.
- [ ] `go-jose/v4` promoted from indirect to direct in `go.mod`.
- [ ] `pkg/edgejwks` does NOT leak `go-jose` types outside the package
  boundary (cmd-side sees `edgejwks.Cache` interface, not `*jwk.Cache`).
- [ ] `HostEntry.Put` no-op guard widened to 7 slices.
- [ ] Compile-time interface check at `cmd/gatewayd-internal/edge_rules.go`
  still passes.
- [ ] No new migrations (schema unchanged from PR 1).
- [ ] `make lint` introduces zero new issues.
- [ ] `codeql go` clean.

## Rollback

Revert the 6 commits in reverse. `pkg/edgejwks` package is purely additive (no
schema, no shared type changes outside the new package). `pkg/api/dto.go` keeps
the `https://` prefix check from PR 1; reverting PR 5 only drops the network-
position guard and HS\* restriction (a slight loss, but the seed `https://`
check still applies). `cmd/gatewayd-internal/edge_rules.go` widens back to
4-kind compile + match — PRs 3/4 keep their `kind∈{route,rewrite,redirect,
headers}` surface intact.

The cite-bug sweep is a **docs change only** — a partial revert would leave
some edge-rules code comments citing "ADR-089". A clean revert restores the
broken citations; the next ADR to land in 092-099 should re-do the sweep.

## Cross-cutting notes

- **HS\* migration ticket (out of PR 5 scope):** a one-time SQL audit
  `select id, app_id, action->>'algorithms' from edge_rules where action->>'algorithms' like '%HS%'`
  should run on production before PR 5 lands. If any rows return, they need a
  manual data-migration to a different algorithm (no automatic conversion is
  possible — HS\* without a shared secret is structurally broken). The PR
  description flags this as a deployment-prep ticket.
- **CORS `*`+credentials migration ticket:** the same audit logic applies
  (`action->>'allow_origins' = '["*"]' and action->>'allow_credentials' = 'true'`).
  No rows are expected in the seed environment; production audit closes the
  loop before PR 5 ships.
- **CORS `*`+credentials footgun history:** the legacy gateway monolith had a
  similar stamper that emitted `*` indiscriminately and ship-blocked
  credentials-rich apps. The current Tier A7 split puts the stamper behind
  `pkg/gateway/handler.go::applyEdgeRuleCORS`, where the validated-input
  posture mirrors the §17 G3 lesson.
- **No resource-scope change:** edge rules remain **per-app**, not per-org.
  The org-scope resolution (ADR-190 PR-4) doesn't extend them.

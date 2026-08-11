# ADR-093 — opt-in per-route observability inside an app (gatewayd-internal)

Status: Accepted, 2026-08-11. Owner: @poyrazK.
Related: ADR-042 (per-app metrics — §1 partially superseded by this ADR);
ADR-036 (in-memory reader + bounded Prometheus rollups — the precedent this
ADR extends); ADR-040 (per-account rate limit — closed-set pre-instantiation
precedent); ADR-091 (edge rules — the path-aware matcher that weakened
ADR-042 §1 argument (a)); ADR-080 (raw-bytes bridge — the `app.WebSocket*`
two-level operator/per-app flag pattern).

- **Partially supersedes:** ADR-042 §1 only. The `{app, class}` histogram
  and `gateway_cold_boot_total` rename are preserved verbatim. Only the
  "drop the `route` label" decision is overturned for opt-in apps.

## 1. Context

ADR-042 §1 dropped the `route` label from `gateway_request_duration_seconds`
with two arguments:

> (a) gatewayd is an opaque reverse proxy and never inspects the request
> path. Adding the label would require introducing a route concept from
> scratch, which is well outside the issue's scope.
> (b) Customer URL paths are strictly worse than instance ids: unbounded
> *and* attacker-influenced.

Argument (a) has weakened since ADR-042 landed. The Tier A7 edge-rules work
(ADR-091) introduced path-aware matching into `gatewayd-internal` —
`pkg/gateway/handler.go:857/928/1018/1084/1155/1286/1372` now read
`r.URL.Path` for `MatchRoute`, `MatchRewrite`, `MatchRedirect`, etc. The
"no route concept" claim no longer holds; the gateway *has* a path-aware
vocabulary.

Argument (b) survives unchanged. Customer-supplied paths are attacker-
influenced: a `/users/{uuid}` route under traffic produces unbounded
distinct labels. Argument (b) is the reason this ADR is *opt-in* and
*bounded*, not a reason to never do per-route detail at all.

Issue #273's customer-facing intent — "which of my endpoints is slow" —
is unaddressed today. The customer dashboard exposes
`gateway_request_duration_seconds{app,class}` (per-app, 4-class rollup)
and `gateway_requests_total{app,plan,code}` (per-app, full-code counter),
neither of which answers the question. Customers hosting APIs have to
instrument their function themselves to get the per-route breakdown.

## 2. Scope

Adds opt-in per-route observability for customers who enable it on a
specific app. Two surfaces, kept consistent:

1. **Prometheus series on the gatewayd-internal control listener
   `/metrics`** — the existing scrape endpoint.
2. **Bounded in-memory reader** at
   `GET /v1/internal/apps/{slug}/routes` on the same control listener,
   reverse-proxied by apid as `GET /v1/apps/{slug}/routes`. Drives the
   per-app dashboard panel.

The existing `{app, class}` histogram and `{app, plan, code}` counter
remain unchanged for all apps (opt-in or not). The new series are
**additional**, not replacements.

### Out of scope (deferred)

- **Per-app range selector** on the dashboard (`?range=`) — see ADR-042
  "What this PR does NOT add". Same follow-up applies; ADR-093 does not
  introduce the UI control.
- **Per-route sampling** (e.g. record 1-in-N requests to bound cost
  further) — ADR-093 emits every request. A future ADR can layer
  sampling on top if the per-app cap is exceeded in practice.
- **Per-route cost attribution** (egress bytes per route, cold boots
  per route) — the streaming/egress metrics stay `{app, plan}` keyed.
- **Sidecar-route breakdown** — the `--sidecar` selector (ADR-069) is
  not in this ADR's label set; sidecars share the parent app's route
  label. A future ADR can extend.

## 3. Decisions

### D1. Opt-in per app, gated by plan, with operator kill-switch

Mirror the `websocket_enabled` opt-in pattern (ADR-080 / migration 00155)
end-to-end:

- New column `apps.route_metrics_enabled boolean NOT NULL DEFAULT false`
  + partial index `WHERE route_metrics_enabled = true`.
- Plan gate: `pkg/api/limits.go` gains `Plan.RouteMetricsAllowed()`
  mirroring `Plan.WebSocketEnabled()` at the same site. Limits table
  rows: Free = false, Hobby = true, Pro = true, Scale = true. Free-tier
  attempts return `plan_route_metrics_not_allowed` (403).
- Operator-level kill-switch on `cmd/gatewayd-internal/config.go`:
  `[route_metrics] enabled = false` (default). The two flags are
  AND-gated — the operator kill-switch wins.
- Propagation: rides the existing `db.NotifyAppChanged` pg_notify
  channel (cmd/gatewayd-internal/backend.go:193/255) and the existing
  `routes.Reset()` wholesale cache flush. No new notifier.

The two-level AND gate ensures a misconfigured fleet (operator off,
customer on) emits nothing, and a tier-restricted rollout (operator on,
Free customer) returns a deterministic 403 at the API edge.

### D2. Route label is `method + raw path`, bounded per app

The label is computed as `r.Method + " " + r.URL.Path` — verbatim path
including any param values — at the point `Backend.Lookup` resolves
the app, **before** any ADR-091 `kind=rewrite` edge rule mutates
`r.URL.Path` at handler.go:973/976. Documented as D2.3 below.

Per-app cardinality is bounded by `route_metrics_per_app_cap = 50`
(constant in `pkg/api/limits.go`). Beyond the cap, all labels collapse
into a single reserved bucket `__route_other__` (non-evicting, mirror
of `account_label_set.go:42-103` and `hostname_label_set.go:34-43`).
The map size is bounded deterministically: `50 + 1 = 51` entries per
opt-in app at any time.

**Why `method + raw path` (chosen over parameter collapse / first-
segment only):** the user explicitly chose this shape; the cap +
`__route_other__` bucket contain the blast radius. UUID-style traffic
(per `/users/{uuid}`) trips the cap after 50 distinct UUIDs and the
overflow becomes a *signal*: a sustained spike in `__route_other__` on
an app means "this app has a wildcard route", which is exactly what
the operator wants to see.

**Reserved labels admitted free** (no cap cost):
`__route_other__` (the overflow bucket itself) plus the empty string
(used when `appID == ""` and the request didn't resolve — the
equivalent of the existing `"-"` sentinel for `app`/`plan`).

### D3. Bounded in-memory reader is the authoritative detail

The control-listener reader is the *source of truth* for per-route
detail. Prometheus series are a *projection* of the same data through
the cap. This is the exact pattern ADR-036 cited for instance stats:

> per-instance detail lives in the in-memory `pkg/sched/instancestats.Reader`
> while Prometheus gets bounded rollups (spec §12).

ADR-042 §1 cited this same pattern as the reason to drop the label.
ADR-093 extends the pattern: roll up the *count* through Prometheus
(`{app, route, code}`) and serve the *detail* through the reader
(`{app, route, count, p50, p95, p99}` per row). The reader's
`routeLabelSet` is the same non-evicting map the Prometheus side
references — both surfaces see the same cap.

### D4. Two new Prometheus series + one new counter

Registered on `gatewayd-internal`'s existing `/metrics` endpoint (the
control listener, loopback-only on `FAAS_GATEWAY_CONTROL_LISTEN`):

| Series | Labels | Type | Per-app series cap |
|---|---|---|---|
| `gateway_requests_total` | `{app, plan, route, code}` | Counter | 50 × 60 × ~5 = ~15,000 (unchanged from the existing 4-class rollup × N apps, distributed over 60 codes) |
| `gateway_request_duration_seconds` | `{app, route, class}` | Histogram (11 buckets + `_sum` + `_count`) | 50 × 4 × 14 = **2,800** per opt-in app |
| `gateway_request_failures_total` | `{app, plan, route, code}` | Counter | subset of `gateway_requests_total` (status ≥ 400) |

Per-app pre-instantiation: at `Backend.Lookup` time, when
`App.RouteMetricsEnabled && operatorEnabled`, the handler
pre-instantiates the closed `class` set per admitted route (mirroring
`Metrics.PreInstantiateApp` from ADR-042 §2) via a new
`Metrics.PreInstantiateAppRoute(appID, route)` helper, deduped via
`sync.Map`. The `route` label itself is admitted on first observation
through `routeLabelSet.Admit(route)` — bounded to 50.

The pre-existing `{app, class}` histogram and `{app, plan, code}`
counter continue to emit for opt-in apps (the customer dashboard
relies on them; ADR-042's contracts are preserved).

### D5. New control-listener endpoint + apid reverse-proxy

On `gatewayd-internal`'s loopback control listener (existing
`FAAS_GATEWAY_CONTROL_LISTEN=127.0.0.1:9090`, run.go:128):

```
GET /v1/internal/apps/{slug}/routes
```

Mounted alongside the existing `/v1/internal/quota` (run.go:1312-1317).
Reads the same `routeLabelSet` map that the Prometheus side uses, so
the two surfaces are *consistent by construction* — they share the
underlying map, not a separately-computed view.

apid reverse-proxies this path via the existing `apidProxy`
(cmd/gatewayd-internal/proxy.go:141-145) at:

```
GET /v1/apps/{slug}/routes
```

mounted on `cmd/apid/server.go:785` next to
`GET /v1/apps/{slug}/metrics`, behind
`s.authLimited(s.requireScope(api.ScopesReadSurface...))`. IDOR-safe
via the existing `loadApp` helper. No MFA.

Response shape (additive on `AppMetricsResponse` in
`pkg/api/dto.go:2713`):

```go
type RouteRow struct {
    Route     string  `json:"route"`              // "GET /users/4f8a" or "__route_other__"
    Count     uint64  `json:"count"`              // requests in window
    P50MS     float64 `json:"p50_ms"`
    P95MS     float64 `json:"p95_ms"`
    P99MS     float64 `json:"p99_ms"`
    ErrorPct  float64 `json:"error_pct"`          // code ≥ 400 / total
}

type AppMetricsResponse struct {
    // ... existing ADR-042 fields ...
    Routes    []RouteRow `json:"routes,omitempty"` // empty when route_metrics_enabled=false
}
```

### D6. Edge-rule rewrite does not shift the route label

ADR-091's `kind=rewrite` edge rule mutates `r.URL.Path` in place at
handler.go:973/976 *before* forwarding. ADR-093 derives the route
label at the top of `ServeHTTP`, immediately after `Backend.Lookup`
resolves the app and before any rule matcher runs. The label is then
stashed on `r.Context()` via a new `WithRouteLabel(string)` (mirror
of `WithSidecarPort` in `pkg/gateway/observability.go:36-40`) and
read by `Handler.observe()` on the single exit funnel.

Consequence: if a customer has a rewrite from `/v1/foo` → `/v2/foo`,
the route label is `GET /v1/foo` (pre-rewrite). This is the customer-
facing identity and matches the dashboard's "endpoint" intuition.
The ADR-042 forwarder (forwardproxy.go:295-301) still sends the
rewritten path to the guest — the label and the wire diverge
deliberately.

### D7. Recording rule + alert reuse the apid per-route precedent

New entries in `deploy/ansible/roles/prometheus/files/faas.rules.yml`
mirror the existing apid per-route pattern (lines 45/63/68-72):

```yaml
# Per-route rate (opt-in apps only).
- record: faas_gateway_request_rate_5m:by_route
  expr: sum by (app, route) (rate(gateway_requests_total[5m]))

# Wildcard-route detector: sustained overflow into __route_other__.
- alert: GatewayWildcardRoute
  expr: |
    sum by (app) (rate(gateway_requests_total{route="__route_other__"}[10m])) > 1
  for: 10m
  labels: { severity: info }
  annotations:
    summary: "App {{ $labels.app }} has a wildcard route"
    description: "Route label collapsed into __route_other__ for >10m. This app likely
      serves /users/{uuid} or similar parameterised paths. Either the per-app route cap
      (50) is exceeded or the customer wants to enumerate routes explicitly (follow-up
      ADR)."
```

The alert is `severity: info` because it is a customer-experience
signal, not a platform problem.

## 4. Files (New)

| Path | Purpose |
|---|---|
| `migrations/00212_apps_route_metrics_enabled.sql` | `ADD COLUMN IF NOT EXISTS route_metrics_enabled boolean NOT NULL DEFAULT false` + partial index. Cites 00155 as precedent. **Re-verified at PR-open: PR #836 holds slot 211; this PR takes 212.** |
| `pkg/gateway/route_label_set.go` | Mirror of `pkg/gateway/account_label_set.go` (cap=50, non-evicting, `__route_other__` overflow, reserved labels admitted free). |
| `pkg/gateway/route_label_set_test.go` | Property test: fuzz 10k random paths through one app, assert `≤ 51` distinct labels emitted. Mirror `pkg/wire/metrics_cardinality_test.go` shape. |
| `pkg/gateway/control_routes.go` | Control-listener handler `(*Handler).handleAppRoutes` for `GET /v1/internal/apps/{slug}/routes`. |
| `pkg/gateway/control_routes_test.go` | Whitebox exposition-scrape test, mirrors `handler_test.go:400-407`. |
| `pkg/gateway/observe_route_test.go` | Whitebox: `Handler.observe` picks up the route label from context; extends `metrics_test.go:95 TestMetricsPreInstantiateAppBounded` with `TestMetricsPreInstantiateRouteBounded`. |
| `cmd/e2e/per_route_metrics_e2e_test.go` | API-shape e2e (no KVM). Asserts: flag off → empty `Routes`; Free plan → 403 `plan_route_metrics_not_allowed`; cap exceeded → 50 routes + 1 `__route_other__`. Mirrors `cmd/e2e/account_scoped_e2e_test.go:452`. |
| `cmd/e2e/per_route_metrics_metal_test.go` | `//go:build metal`. Scrapes control listener directly. Mirrors `cmd/e2e/deploy_wake_metal_test.go:454`. |

## 5. Files (Modified)

| Path | What changes |
|---|---|
| `pkg/gateway/metrics.go` | Extend `Metrics` struct: `requestsByRoute`, `durationByRoute`, `failuresByRoute`. Add `ObserveRequestRoute`, `ObserveRequestDurationRoute`, `RequestFailureRoute`. Add `PreInstantiateAppRoute(appID, route)` for the closed `class` set per admitted route. |
| `pkg/gateway/handler.go:48` | `App` struct gains `RouteMetricsEnabled bool`. |
| `pkg/gateway/handler.go` | Two changes only: (1) `Handler` gains a per-app `routeLabelSet` lazily created from `App.RouteMetricsEnabled`; (2) `Handler.observe` at line 2745 accepts a `route string` arg, populated from `r.Context()` (set by a new `WithRouteLabel` write at the top of `ServeHTTP`, immediately after `Backend.Lookup`). The 8 `observe()` call sites pass `""` when `appID == ""`. |
| `pkg/gateway/observability.go` | Add `routeLabelKey` / `WithRouteLabel` / `RouteLabelFromContext` (mirror of `WithSidecarPort` at lines 36-40). |
| `pkg/gateway/forwardproxy.go` | No change. `ForwardHTTPRequestInit.RequestUri` stays verbatim (post-rewrite). Route label is pre-rewrite per D6. |
| `cmd/gatewayd-internal/backend.go:109,118` | App mapping sites populate `RouteMetricsEnabled` from the resolved app row. |
| `cmd/gatewayd-internal/run.go:521,971-972` | Second mapping site mirrors backend.go. |
| `cmd/gatewayd-internal/run.go:1312-1317` | Wire `/v1/internal/apps/{slug}/routes` into the loopback control mux. |
| `cmd/gatewayd-internal/config.go` | Add `[route_metrics] enabled` operator kill-switch. |
| `cmd/gatewayd-internal/run.go:718` | Env merge for the kill-switch. |
| `pkg/api/limits.go` | Constant `route_metrics_per_app_cap = 50`. `Plan.RouteMetricsAllowed()` mirroring `Plan.WebSocketEnabled()`. Limits rows: Free=false, Hobby=true, Pro=true, Scale=true. |
| `pkg/api/dto.go` | `CreateAppRequest` + `UpdateAppRequest` gain `RouteMetricsEnabled *bool`; `AppResponse` gains `RouteMetricsEnabled bool`. `AppMetricsResponse` gains `Routes []RouteRow`. |
| `pkg/api/openapi.yaml` | Add `route_metrics_enabled` + `/v1/apps/{slug}/routes` route + `RouteRow` schema with `x-since: "2026-08"` + `x-issue: "273"`. **`make spec-sync` required after this edit (memory: spec-sync-stale-embed-on-openapi-change).** |
| `pkg/apid/openapi.yaml` | Regenerated by `make spec-sync`. |
| `pkg/state/pgstore.go` | Add `route_metrics_enabled` to `CreateApp` column list (memory: pgstore-createapp-column-list-must-match-apisurface). Add `SetRouteMetricsEnabled` companion. |
| `pkg/state/memstore.go:2874-2886` | Memstore equivalent. |
| `cmd/apid/server.go:785` | New route `GET /v1/apps/{slug}/routes` mounted behind `s.authLimited(s.requireScope(api.ScopesReadSurface...))`. Reverse-proxies to gatewayd control listener via existing `apidProxy`. |
| `cmd/apid/handlers_metrics.go:37` | `getAppMetrics` extended to fetch the per-route rows and return them in `AppMetricsResponse.Routes`. |
| `deploy/ansible/roles/prometheus/files/faas.rules.yml` | New recording rules + `GatewayWildcardRoute` alert per D7. |
| `deploy/grafana/faas-fleet.json:386-476` | New panels: "per-route traffic" and "wildcard route detection (__route_other__ spike)". |
| `docs/adr/042-per-app-metrics-and-cold-boot-rename.md:1-17` | Add `**Partially superseded (in §1 only) by:** ADR-093` banner after the existing PR-E banner. Body preserved verbatim. |
| `docs/adr/README.md` | Add ADR-093 row to the log table; update ADR-042's row to note partial supersession. |
| `docs/faas_implementation_spec.md:780-786` | (1) Replace "ADR-041" with "ADR-042" — the spec has mis-cited the ADR since it landed (pre-existing doc bug). (2) Add a §12.6 sidebar referencing ADR-093. Per CLAUDE.md, spec deviation requires an ADR; the spec-sync status check (ADR-085) will fail the PR if §12 drifts without the ADR — both must land together. |

## 6. Consequences

### Positive

- Customers hosting APIs can answer "which endpoint is slow" from the
  platform, with deterministic bounds (50 routes per app, `__route_other__`
  overflow signal).
- Two surfaces (Prometheus + control listener) sharing one underlying
  map — no drift risk between dashboard and metrics.
- ADR-042's `{app, class}` histogram and the rename to
  `gateway_cold_boot_total` are preserved. No backward incompatibility.
- Pre-existing per-app pre-instantiation pattern (ADR-040, ADR-042 §2)
  is reused — the new metric fits the existing architecture, no new
  cardinality budget framework needed.

### Negative

- Per opt-in app, 50 routes × 14 series × class = ~2,800 new histogram
  series, plus up to ~15,000 new counter series (50 × 60 codes × 5
  plans, distributed). At ~100 opt-in apps (a generous estimate for
  M8) this is ~280k new histogram series + ~1.5M counter series
  globally — still well under Prometheus norms but a meaningful
  chunk of the §12 budget. **Mitigation:** the operator kill-switch
  (D1) bounds the customer opt-in side; the cap + `__route_other__`
  bounds the per-app side; the alert (D7) surfaces the "wildcard
  route" condition so an operator can intervene.
- Two surfaces to keep consistent. **Mitigation:** they share the
  same underlying map (D3); the reader is the source of truth.
- New operator knob in `cmd/gatewayd-internal/config.go`. The default
  is off, so existing deployments are unaffected.

### Out of scope (deferred)

- Per-app range selector on the dashboard (carried over from ADR-042).
- Per-route sampling.
- Per-route cost attribution (egress bytes, cold boots).
- Sidecar-route breakdown.

### Compatibility

- Existing `GET /v1/apps/{slug}/metrics` response gains an additive
  `Routes` field (omitempty). Old clients see no change.
- Existing Prometheus series are unchanged. Old dashboards see no
  change.
- `apps.route_metrics_enabled` defaults to `false`. No app's behaviour
  changes without an explicit PATCH.
- Spec §12:780-786 is fixed in-place to correct the ADR-041 mis-cite
  and add the ADR-093 sidebar. The dashboard panel table is unchanged.

## 7. Acceptance

1. New migration `00212` (slot re-verified at PR-open) lands with
   `route_metrics_enabled` boolean + partial index; existing
   `apps.websocket_enabled` migration is the cited precedent.
2. `pkg/gateway/route_label_set_test.go` property test: under fuzzed
   10k random paths through one opt-in app, ≤ 51 distinct labels
   emitted. Fails CI if the cap regresses.
3. `metrics_test.go:95 TestMetricsPreInstantiateAppBounded` is
   extended with `TestMetricsPreInstantiateRouteBounded` proving the
   pre-instantiation contract holds for the new series.
4. `cmd/e2e/per_route_metrics_e2e_test.go` (no KVM) asserts:
   (a) flag off → `Routes` array empty;
   (b) Free plan → 403 `plan_route_metrics_not_allowed`;
   (c) cap exceeded → exactly 50 distinct `route` entries + one
   `__route_other__` bucket.
5. `cmd/e2e/per_route_metrics_metal_test.go` (`//go:build metal`)
   asserts: 1k requests at `/users/{uuid}` patterns → exactly one
   `__route_other__` series per `{app, class}` tuple on the control
   listener scrape; `GET /v1/internal/apps/{slug}/routes` returns the
   same 50+1 breakdown.
6. `apid`'s `GET /v1/apps/{slug}/routes` (via `apidProxy`) returns
   the same shape as #5.
7. `pkg/api/openapi.yaml` regen + `make spec-sync` lands in the same
   PR, separate commit.
8. ADR-042 edit (partial supersession banner) + ADR-093 file +
   `docs/adr/README.md` index row + spec §12:780-786 fix land in the
   same PR (coupled set).

## 8. Rejected alternatives

| Alternative | Why not |
|---|---|
| **Per-route explicit allowlist** (customer enumerates routes in `apps.route_metrics_config jsonb`) | Strictly bounded by the customer's list, no `__other__` needed. Rejected because it requires the customer to know their route shape up front, which defeats the dashboard's "discover your slow endpoints" intent. The cap + `__route_other__` shape is opt-in *and* discovery-friendly. |
| **Parameter collapse** (`/users/4f8a` → `/users/{id}`) | Bounded and high-fidelity. Rejected because the user explicitly chose `method + raw path`; the cap protects cardinality; the `__route_other__` bucket becomes a useful "wildcard route" signal. A future ADR can layer collapse on top if the cap trips in practice. |
| **Prometheus only, no control listener** | Simpler mechanically. Rejected because it puts the entire dashboard behind a Prometheus scrape, which couples the customer-facing UI to a Prometheus availability; the in-memory reader pattern (ADR-036) gives a guaranteed-latency path. |
| **Control listener only, no Prometheus series** | Reverses ADR-042's dashboard pattern. Rejected because fleet-level dashboards and existing per-app dashboards already use the Prometheus side; removing it would orphan panels. Both surfaces, one map. |
| **Coexist with ADR-042 (no supersession)** | Less paperwork. Rejected because the historical contradiction ("ADR-042 §1 says drop the label; ADR-093 §1 says add it") is on the page; partial supersession is the canonical ADR precedent (042 itself, 041-tenant-abuse, 075-eviction-priority, 077-step-up-mfa, 032-paddle-billing-provider). |
| **Free tier opt-in** | Rejected on cost grounds: Free-tier observability is intentionally coarse (§12), and the new path adds cardinality that Free customers would not have budget for. Hobby+ only. |

## 9. References

- Issue #273 — original per-app + per-route request.
- ADR-036 — in-memory reader + bounded Prometheus rollups (the
  precedent this ADR extends).
- ADR-042 — per-app metrics (partially superseded §1).
- ADR-040 — per-account rate limit (closed-set pre-instantiation +
  `__other__` placeholder).
- ADR-091 — edge rules (path-aware matcher that weakened ADR-042 §1
  argument (a)).
- ADR-080 — raw-bytes bridge (the `websocket_enabled` two-level
  operator/per-app opt-in pattern).
- Spec §12 — Prometheus dashboards (lines 780-786 are fixed in this PR).
- Spec §4.1 — gatewayd rate-limit contract (unchanged).
- Spec §11 — abuse-vector observability (unchanged — `__route_other__`
  is the same overflow sentinel; nothing new is exposed to attackers).

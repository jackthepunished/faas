# ADR-040 — per-tenant noisy-customer gauge + FaasTenantAbuse alert (issue #300)

Status: Accepted, 2026-07-27. Owner: @poyrazK. Closes: #300.
Related: #278 (per-customer failure observability, the
`apid_request_total` / `apid_request_failures_total` counter pair
and the `accountLabelSet` admission primitive — issue #300 sits
on top), #303 (traffic anomaly detection, ADR-039 — the `mode ×
direction × metric` taxonomy this ADR extends; the new
`family: tenant_abuse` slot is additive and composes with the
existing `family`-based inhibition rules).

## Context

The §12 traffic-anomaly rules (ADR-039) answer "is anything wrong
across the fleet?" but they don't surface a single load-bearing
question for the operator: **"which one customer is hammering us
right now?"**. There is no per-tenant fixed-threshold alert. A
noisy customer hitting 600 rps — well below the spike-detection
threshold of 3× baseline on a route that routinely sees 200 rps —
is invisible to today's alerts because no individual route crosses
its baseline.

Issue #300 asks for:

1. A `apid_top_tenant_rps{account_id}` gauge sampled at 5s, with
   `account_id` cardinality bounded to top-1000 by 24h request
   count and the rest folded under `account_id="other"`.
2. A `gateway_top_tenant_rps{account_id}` gauge, parallel to
   apid's, sampled at the same cadence.
3. A Grafana panel "Top-10 noisy customers (5m)" on a new
   dashboard (`dashboards/top-tenants.json`).
4. A `FaasTenantAbuse` alert (`apid_top_tenant_rps > 500` for 10m,
   `severity=warn`), with response = rate-limit + notify,
   escalation = suspend deployment.
5. A runbook at `docs/runbooks/tenant-abuse.md`.
6. A property test that asserts `apid_top_tenant_rps` cardinality
   never exceeds 1001 across fuzzed load.

The lock-down: this PR ships the **literal** acceptance list above.
The follow-on per-account anomaly-score alerts (the ADR-039 gap I
identified at §Open follow-ups) stay out of scope; that work is
explicitly deferred to a future PR.

## Decision

Introduce a bounded-cardinality gauge primitive, a sampler
goroutine that drives the gauge emission from a single goroutine
on a 5s tick, an alert keyed off the gauge, a dashboard, a runbook,
and a synthetic-fixture test. The primitive lives in `pkg/wire`;
the sampler lives in `cmd/apid` (and a parallel mirror in
`cmd/gatewayd`); the alert, dashboard, and runbook live in their
existing repos.

### Two-tier cardinality bound (issue #300 acceptance #4)

`apid_top_tenant_rps` cardinality is bounded at top-1000 + 1
"other" overflow bucket. This is a presentation view layered above
the deeper 10 000 + `__other__` bound on the underlying
`apid_request_total` counter (issue #278). The two bounds are
deliberately separate because they serve different consumers:

- The counter (10 000 + `__other__`) is the source of truth. Its
  bound protects the TSDB series set from being consumed by
  dormant or one-shot accounts.
- The gauge (1000 + "other") is a top-N view keyed on the last
  24h's request count. Its bound is tighter because the operator
  only ever reads the top of the list — the dashboard's
  "Top-10 noisy customers" panel doesn't care about the 8001st
  account, and the alert's `topk(20, ...)` aggregator explicitly
  caps at 20.

The two overflow labels are intentionally distinct — `__other__`
on the counter, `other` on the gauge — so a Grafana panel can
filter one without filtering the other. Conflating the labels
would force operators to choose between filtering the overflow
bucket at the gauge layer (legitimate) and at the counter layer
(lossy — would hide demoted customers from the per-account
failure views).

### Sampler goroutine, not per-request gauge writes (issue #300 acceptance #1)

The gauge emission happens once per 5s tick from a single
goroutine (`cmd/apid/topn.go`, mirrored by `cmd/gatewayd/topn.go`
on the gateway side). The per-request path only bumps the
rolling-window count via `OpsMetrics.ObserveTopTenantRPS(id)`;
the sampler computes the diff and calls `EmitTopTenantRPS` to
drive the gauge.

This split is load-bearing. Pushing the gauge write to a single
goroutine keeps the series set bounded at cap+1 deterministically.
A per-request gauge Set would accumulate series for every id that
ever transiently held a top-N slot — under concurrent sampling,
the top-N membership bounces for any given id as more ids
arrive. Every id that briefly held a top-N slot would create a
gauge row, blowing past the cardinality bound within seconds.

The split is also explicitly about the 5s cadence. Faster ticks
(1s) burn CPU on the sort for no panel fidelity gain; slower
ticks (30s) let a noisy customer slip between samples.

### 24h rolling window (not lifetime, not daily-snapshot)

A 24h rolling window strikes the balance between the two failure
modes:

- **Lifetime view** would let a one-shot noisy customer persist
  in the top-N forever; an account that flooded the platform in
  February would still occupy a top-N slot in August, dragging
  the view.
- **Daily-snapshot view** (midnight-UTC reset) would let a quiet
  customer jump into the top-N at midnight regardless of
  activity — the top-N would be dominated by accounts whose
  traffic spikes at the boundary, not by truly noisy customers.

The 24h rolling reset is driven by the sampler
(`topAccountSet.shouldReset()` checks the wall clock and calls
`resetWindow()` when the window has elapsed). The reset wipes the
counts map; gauge rows for ids that left the top-N go to 0
(Prometheus gauges don't have rows to delete).

### Gateway gauge: label key `account_id`, label value is `app_id`

`gateway_top_tenant_rps` shares the label key `account_id` with
apid's gauge so a single Grafana panel can join both surfaces —
but the label VALUE at the gateway is the resolved `app_id`, not
an authenticated principal. Gatewayd is pre-auth (TLS termination
+ hostname routing only); the only tenant-attributable key on the
request path is the app_id. The apps table's owner (account_id)
lives in apid's domain.

Operators reading the panel should treat
`gateway_top_tenant_rps` as "noisy apps seen at the edge" and
`apid_top_tenant_rps` as "noisy customers on the API". The
dashboard's panel 2 ("Top-10 noisy apps (5m, gateway)") labels
this distinction in the panel title.

### Alert shape: `topk(20, max(...) by (account_id)) > 500`

The alert uses `topk(20, max(apid_top_tenant_rps{account_id!="other"})
by (account_id)) > 500` for 10m. Three design choices:

1. **`topk(20, ...)` over `max(...)`.** The aggregator preserves
   the `account_id` label so Alertmanager routes per-customer;
   the page receiver groups by `[alertname, component]` so 20
   simultaneous offenders collapse into one page with
   per-account drill-down. A bare `max(...)` would strip the
   label and turn every abusive customer into one anonymous page.

2. **`{account_id!="other"}` inside the expression.** The
   `!=` matcher is part of the metric selector — not a label
   filter on the alert. Without it, `topk` would happily pick
   the overflow bucket as one of its top-20, and a saturated
   top-1000 cap would trip the alert on its own. The overflow
   bucket represents saturated admission (a fleet-level signal),
   not abusive behavior (a customer-level signal); separating
   them is the point of the two-tier bound.

3. **10m `for:` window.** A noisy customer's first 5m burst
   might be a legitimate campaign; 10m is the debounce that
   lets the customer self-correct before page. The
   `pkg/promqlrules/testdata/tenant_abuse.test.yml` fixture pins
   this with the bursty-customer test (5m at 1500 rps, drops to
   50 rps for 20m → alert does NOT fire).

### Routing: `severity=warn → faas-warn`

The alert follows the existing `severity` label routing
(`deploy/ansible/roles/alertmanager/templates/alertmanager.yml.j2:48-51`).
The `family: tenant_abuse` label is the new slot in the existing
`family`-based inhibition / silencing rule
(`alertmanager.yml.j2:106-110`) — same shape as `family: traffic_anomaly`,
no new routing infrastructure.

The `account_id` rides on the alert payload as a drill-down label;
Alertmanager routes the page, the runbook drills into the customer.

### Gateway sampler: local topAccountSet, not pkg/wire

The cmd/gatewayd sampler runs a private mirror of the
`topAccountSet` primitive rather than importing pkg/wire's.
Reasoning: pkg/gateway can't import pkg/wire (the cycle would be
cmd/gatewayd → pkg/gateway → pkg/wire → cmd/gatewayd; pkg/gateway
predates pkg/wire and is intentionally narrow). The two
primitives are NOT kept in sync by tests (no shared test surface);
the integration is pinned at the dashboard/alert level
(FaasTenantAbuse uses both gauges and the synthetic-fixture
test exercises the contract).

A future refactor that lifts pkg/wire into a position where
pkg/gateway can import it should collapse the two primitives
into one. Tracked as follow-up; not in scope for this PR.

## Consequences

### Positive

- Operators have a single, fixed-threshold signal for the most
  common abuse pattern (a runaway retry storm from one customer).
  The §12 traffic-anomaly rules cover the rarer fleet-wide
  patterns; FaasTenantAbuse covers the per-customer one.
- The top-N view keeps the TSDB series set bounded at cap+1,
  regardless of customer growth. A platform with 50 000 customers
  emits the same gauge series count as a platform with 1 000.
- The synthetic-fixture test pins the alert's contract without
  requiring 3d of synthetic data (the precedent set by
  `pkg/promqlrules/testdata/anomaly_score.test.yml`).
- The `severity=warn → faas-warn` routing composes with the
  existing family-based inhibition / silencing — a customer
  silenced by another tenant-abuse alert silences this one too,
  and vice versa.
- The byte-identical dashboard invariant (`pkg/grafana/dashboard_test.go`)
  catches deploy/ansible drift at PR time, not at provisioning time.

### Negative

- The gauge is a presentation view; if the sampler goroutine
  crashes (or its ctx is cancelled mid-shutdown), the gauge
  values stop updating while the underlying counter continues
  to increment. The dashboard would show stale data. This is
  acceptable: the gauge is a presentation view, the counter is
  the source of truth, and Prometheus's `for:` window on the
  alert would catch a sustained stale-data condition within
  10m. A future refactor could lift the sampler into the same
  process supervisor as the counter (e.g. a single Process group
  with shared liveness); not in scope for this PR.
- The gateway gauge labels `app_id`, not `account_id`. Operators
  reading the panel need to know which is which. The panel
  title distinguishes them ("Top-10 noisy customers (5m, apid)"
  vs "Top-10 noisy apps (5m, gateway)"); the alert docs note
  the distinction in `docs/runbooks/tenant-abuse.md`.
- The two-tier bound means operators can never alert on a
  demoted customer via the gauge. The counter is still
  available for ad-hoc drill-down via the underlying
  `apid_request_total{account_id="__other__"}` series, but the
  gauge's view is capped. This is intentional — the alert is
  for top-of-list abuse, not for long-tail triage.
- The sampler goroutine holds a sync.Mutex around the rolling
  counts; under a request flood the lock is briefly contended.
  The lock is held only across the per-tick map copy (a
  microsecond at cap=1000), not across the sort or the gauge
  emission, so the contention is bounded. The
  `TestTopTenantRPS_ConcurrentSample` race-test pins this.
- The sampler requires `cmd/apid/topn.go` and `cmd/gatewayd/topn.go`
  to be added to the daemon lifecycle (the `bgBefore` hook in
  `cmd/apid/main.go`, parallel goroutine in `cmd/gatewayd/main.go`).
  This adds two goroutines per daemon — negligible cost.

### Alternatives considered

- **Per-account alert keyed off `apid_request_total` directly.** A
  simpler design: alert on
  `topk(20, sum by (account_id) (rate(apid_request_total{account_id!="__other__"}[5m]))) > 500`.
  Rejected: would force every operator dashboard to read the
  per-account counter at 5s resolution, blowing the time-series
  cardinality up to 10 000+ series × 5s × N panels. The gauge
  presentation view bounds the cost.
- **Counter-based gauge (e.g. `apid_request_total{account_id} / 5s`).**
  A counter can't divide by an interval to produce a gauge without
  a recording rule per account_id. The gauge-in-Go pattern (this
  PR) avoids the per-account recording-rule explosion.
- **Lifetime top-N (no 24h reset).** Rejected: lets a one-shot
  noisy customer persist in the top-N forever, dragging the
  view over time. The 24h rolling reset matches the §12
  dashboard's "today's noisiest customers" reading.
- **Daily-snapshot top-N (midnight-UTC reset).** Rejected: lets
  a quiet customer jump into the top-N at midnight regardless
  of activity. The 24h rolling reset is the natural middle
  ground.

## Open follow-ups

- **Per-account anomaly-score alerts** (the ADR-039 §Open
  follow-ups I deferred). These would alert on the 3d-trailing
  baseline for a single customer — orthogonal to the
  fixed-threshold FaasTenantAbuse alert shipped here.
- **Unify the gateway + apid `topAccountSet` primitives** into
  a single shared one in `pkg/wire`. Blocked on pkg/gateway
  gaining a pkg/wire dependency (a refactor in its own right).
- **Auto-paging customers** from the platform side. Out of scope
  per the locked decision: this PR ships the alert + runbook,
  not the action.
- **Per-app labels on the alert payload** (so Alertmanager can
  route per-app, not just per-customer). Currently the
  `app_id` rides on the underlying counter (`apid_request_total{account_id, route, code}`)
  but the gauge collapses it. A future PR could lift the gauge
  to expose both labels.
- **Per-region labels** (so a multi-region Gate-A deployment can
  scope the alert). Single-region launch today; out of scope.

## Path corrigendum

Issue #300's path references in the body
(`pkg/wire/metrics.go`, `pkg/wire/topn.go`, etc.) match the
implementation. The dashboard path
(`dashboards/top-tenants.json` in the issue) is realised at
`deploy/grafana/top-tenants.json` (canonical) +
`deploy/ansible/roles/grafana/files/top-tenants.json` (Ansible
copy) — same precedent as `faas-fleet.json`.

## Acceptance mapping

| #300 criterion | Where shipped |
|---|---|
| 1. `apid_top_tenant_rps{account_id}` + `gateway_top_tenant_rps{account_id}` gauges, 5s sampled | `pkg/wire/metrics.go` (registration); `pkg/wire/topn.go` (top-N primitive); `cmd/apid/topn.go` (sampler goroutine); `pkg/gateway/metrics.go` (gateway gauge); `cmd/gatewayd/topn.go` (gateway sampler); `pkg/gateway/handler.go` (per-request bump) |
| 2. Grafana panel "Top-10 noisy customers (5m)" at `dashboards/top-tenants.json` | `deploy/grafana/top-tenants.json` (canonical); `deploy/ansible/roles/grafana/files/top-tenants.json` (Ansible copy) |
| 3. `docs/runbooks/tenant-abuse.md` + `FaasTenantAbuse: apid_top_tenant_rps > 500` for 10m, response = rate-limit + notify, escalation = suspend deployment | `docs/runbooks/tenant-abuse.md` (new); `deploy/ansible/roles/prometheus/files/faas.rules.yml` (new alert block); ADR-040 (this doc) |
| 4. `account_id` cardinality bounded to top-1000 by 24h request count; rest under `account_id="other"` | `pkg/wire/topn.go` (`topAccountSet`); `pkg/wire/metrics.go` (`topTenantRPS` gauge pre-instantiates `("other",)`); `cmd/gatewayd/topn.go` (gateway mirror) |
| 5. Property test asserts `apid_top_tenant_rps` cardinality ≤ 1001 across fuzzed load | `pkg/wire/topn_test.go::TestTopTenantRPS_BoundedCardinality`; `pkg/promqlrules/testdata/tenant_abuse.test.yml` (5 synthetic scenarios) |

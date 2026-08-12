# ADR-095 PR-cluster outline

Five reviewable PRs, each ships behind feature flags, each independently
reviewable in ~10 min. Mirrors the cluster discipline used by ADR-090
(named envs, slot chain 198→203) and ADR-091 / ADR-092 (edge rules,
slot chain ending in 214→218).

| PR | Files touched | Behavior change | Review budget |
|---|---|---|---|
| **PR-0 docs** | `docs/adr/095-*.md`, `migrations/00219_reserve_slot.sql`, `docs/faas_implementation_spec.md` §9.A, `pkg/api/limits.go` + test | None. Spec + ADR + slot fence + quota table only. | ~10 min |
| **PR-A schema** | `migrations/00219_data_upstreams.sql` (+ test, + probes partition), `pkg/state/{types,pgstore,memstore}.go`, `pkg/state/sqlc/queries/*`, `docs/storage.md` | None. Pure DDL; no code path reads the tables yet. | ~10 min |
| **PR-B capture** | `pkg/data/{infer,extract}.go`, `cmd/apid/handlers_upstreams.go`, `cmd/apid/server.go`, `api/openapi.yaml`, `pkg/apid/openapi.yaml`, three SDKs | Captures hinted upstreams from env PUTs + explicit POST. Default OFF (`FAAS_DATA_PLACEMENT=0`). | ~15 min |
| **PR-C probe** | `pkg/meter/upstream_probe.go`, `pkg/meter/loop.go`, `pkg/meter/health.go`, `pkg/wire/metrics.go`, `pkg/promqlrules/data_placement.yaml`, `cmd/meterd/{config,main}.go` | New 30s×5min sliding window probe. Default OFF (`FAAS_UPSTREAM_PROBE=0`). Metric surface added. | ~15 min |
| **PR-D chooser** | `pkg/sched/upstream_affinity.go`, `pkg/sched/placement.go`, `pkg/sched/placement_test.go`, `pkg/sched/engine.go`, `cmd/schedd/main.go` | New `upstream_fit` tie-break in `betterCandidate`. Default OFF (`FAAS_UPSTREAM_AFFINITY=0`). Fails-open to legacy behaviour when no data. | ~20 min |

## Cross-PR gates (every PR satisfies)

Per CLAUDE.md + repo memory (each rule has a memory entry — name in
backticks):

1. `make lint` + `make test` + `make spec-check` (`ci-three-job-split`).
2. `make metal-lima` for any touch to `pkg/fcvm` / `pkg/netns` /
   `vmmd` / `builderd`. PR-D touches `pkg/sched`; metal is regression
   guard only.
3. Every new quota in `pkg/api/limits.go` (`pkg/api/limits_test.go`
   table-driven coverage). No inline numbers.
4. No direct cross-component call. apid writes PG → meterd reads →
   schedd learns via `pg_notify`. No apid→schedd gRPC.
5. Pure-function chooser stays pure (`placement.go` contract).
6. §11 secret rule: env values never logged, only host hashes stored
   in Prom labels.
7. OpenAPI 3.1 + SDK regen (`pr-819-openapi-nullable-3-1`,
   `sdk-coverage-walks-pkg-api`). Three SDKs regenerated.
8. Slot fence discipline (`cross-pr-slot-gate-reservation-fence-pattern`,
   `pr-849-adr-092-pr-a-slot-chase-cluster`).
9. `pkg/apid/openapi.yaml` mirror via `make spec-sync`
   (`spec-sync-stale-embed-on-openapi-change`).

## Rollout gate (cluster-as-a-whole)

Every cluster step is feature-flagged. Defaults OFF for the v1.10
ship. Manual flip per node for v1.11 once CI is clean for one full
month on `main`. The order of flips is **PR-A → PR-B → PR-C → PR-D** —
DDL first, capture second, telemetry third, behaviour last.

The single-node install is byte-identical through v1.10. The
multi-node payoff lands at M9 (`docs/faas_implementation_spec.md` §14
+ ADR-066), gated on the M9 acceptance criteria from the §14
delivery plan.

## Verification (end-to-end)

`cmd/e2e/connection_aware_e2e_test.go` (deferred to the cluster
final-PR — likely a follow-up PR-E or absorbed into PR-D) exercises:

1. `PUT /v1/apps/{slug}/env/DATABASE_URL` with a Neon URL →
   `data_upstreams` row appears within 1 min.
2. `GET /v1/apps/{slug}/upstreams` reflects the row + last observed
   RTT (after PR-C is on).
3. With two `compute_nodes` rows in different regions, the wake
   path's chooser now adds the `upstream_fit` tie-break (after PR-D
   is on).
4. Quota trip returns the new error code on Free apps
   (`data_upstream_quota_exceeded`).
5. Probe worker-pool cap is honoured under burst.

## Out-of-cluster follow-ups

- A9 pressure-rebalance respecting upstream-fit.
- Per-deployment scope overlay (PK widens to
  `(app_id, deployment_scope, kind, host, port)`).
- `GET /v1/apps/{slug}/upstreams/history` time-series endpoint.
- Per-host probe at edge nodes (`gatewayd-public`).
- Static provider→region inference table growth as providers ship
  new region prefixes.
- `gregale inspect <slug> --upstreams` CLI command (mirrors the
  `gregale env list` shape).

# Status

Spec §14 milestones M0 → M8. The README has the one-line version;
this file is the long form (which PR closed which issue, what each
milestone actually shipped, what's left on the board). Update this
when a milestone lands — readers coming from the README land here
for context.

## M0 — repo scaffold. ✅

Repo tree, build/test/lint tooling, CI, `pkg/api` limits table,
8-role ansible bootstrap, hello-boot acceptance test. `make bootstrap`
gates it on a fresh EX44.

## M1 — vmmd core. ✅

Invariant-critical VM lifecycle: slot allocator (`pkg/fcvm`),
per-instance netns/TAP (`pkg/netns`, ADR-009), cold-boot config +
jailer argv (Appendix B / ADR-019), `Manager` with no-leak unwind,
metal layer (`manager_metal_test.go`), and the 5-RPC gRPC surface
at `/run/faas/vmmd.sock` (ADR-013/014/016, `pkg/vmmdgrpc`). KVM +
root required for the metal gate.

## M2 — imaged + guest-init. ✅

OCI→app-layer pipeline, two-drive scheme (`pkg/oci` diff + `pkg/rootfs`
applier), base→ext4 auto-stage (`pkg/imaged::EnsureBaseExt4`), real-mkfs
build in Linux CI, `guest/init` overlay + crash supervisor, two-drive
boot verified metal-side (`cmd/e2e/deploy_wake_metal_test.go`).

**Fixture follow-up:** the body/trim mismatch originally flagged in PR #55
was resolved by PRs #151, #159, #135; `deploy_wake_metal_test.go` is now
exercised by the M8 netns + egress test path. EX44 / Lima sign-off on the
§14 metal acceptance gate is tracked under [What's next](#whats-next).

## M3 — snapshots + wake. ✅

Park/wake with the ADR-005 restore-or-cold-boot fallback, FC version
pinning (`snapshots.fc_version`), and the vsock post-restore resume
hook (ADR-022) that re-seeds entropy + steps clock — V6 acceptance
green in `pkg/fcvm/v6_resume_ext4_metal_test.go`.

**Remaining:** §14 V2 latency loop driver (100 cycles, p50 ≤ 350 ms)
— see [What's next](#whats-next).

## M4 — gatewayd + schedd. ✅

Routing, wake gate, admission ledger (47,600 MB headroom / 160 vCPU),
G7 flow-aware reaper (`pkg/sched/flowcount`), `PGBackend` PG routing,
schedd-over-gRPC (ADR-018), last-seen flush, 1k rps CI-asserted
hot-path load test (PR #44), per-VM `memory.max` + per-plan `tc`
egress (PR #37, closes #31 + #33).

## M5 — apid + deploy pipeline + CLI. 🚧

Production wiring is in via the pgx-backed `state.PgStore`, real
`rootfs.Builder` in `pkg/imaged::handleDeployment` (PR #26),
plan-quota table-tests (`cmd/e2e/quota_e2e_test.go`), the
snapshot-prime handshake that flips a deployment to `live` after
one cold-boot priming cycle, and the G2 sealed-secrets path
(PR #42); `faas` CLI renders RFC 7807 problems (UX §3.3).

**Fixture follow-up:** the body/trim mismatch flagged in PR #55
was resolved by PRs #151, #159, #135 (same fixture exercised by
the M8 netns + egress path). EX44 / Lima sign-off on the §14
metal acceptance gate is tracked under [What's next](#whats-next).

**Beta ship-blockers landed** — PR #136 (`PR-A`, ship-blockers
for the beta cohort) and PR #154 (`PR-B`, atomic supersede +
durable build queue).

**Authentication surface rewritten** — PR #161 added Google
OAuth 2.0 (`cmd/apid/handlers_google.go`, routes mounted at
`cmd/apid/server.go:426-427`); PR #162 auto-creates the user
account and issues an active Bearer API key + session cookie
on signup; PRs #163 and #164 retired the stale magic-link login
email (the `/auth/verify` route remains in the codebase but is
no longer reachable through `/login`); PR #174 hardened
`POST /login` against pre-auth takeover
(`cmd/apid/handlers_auth.go:112`, closes #165).

**CLI SDK promoted** — PR #157 lifted `cmd/faas/client.go` to
`pkg/api/client.go` as the public SDK (38 exported methods
covering apps, deployments, plans, domains, crons, keys,
secrets, usage, and the OAuth device-code flow).

**CLI token now lives in the OS keychain (issue #293, closes gap G5)**
— `cmd/faas/config.go` writes through `github.com/zalando/go-keyring`
(macOS Keychain / Linux libsecret via D-Bus / Windows wincred); the
plaintext file at `~/.config/faas/token` is retained only as a
fallback for headless hosts with no D-Bus session (CI runners,
SSH-only servers), and a WARN recommends installing `gnome-keyring`.
First successful keychain save one-shot-deletes the legacy
plaintext file so customers do not keep a redundant copy on disk
after upgrading.

## M6 — builderd + real image pulls. ✅

Build-in-microVM is wired through (`cmd/builderd`, `pkg/builderd`
orchestration + executor, PRs #39/#40/#43); the metal lifecycle is
in `vm_metal.go` (`//go:build metal`) and calls vmmd over gRPC, with
`vm_stub.go` returning `ErrNotMetal` for non-metal builds. OCI
puller hardened (`pkg/oci/egress.go` — denied CIDRs cover RFC1918,
CGN, loopback, IMDS, ULA), streamed layer blobs. `cmd/imaged`
auto-stages `/srv/fc/base/builder-base.ext4` on startup.

Source-tarball staging + Dockerfile dispatch are in via PR #56
(closes #54): `pkg/builderd/drive.go::CreateBuildDrive1` copies
`VMRequest.SourcePath` into drive1 at `/build/src.tar` and re-stats
a sha256 against the host source to catch torn copies;
`pkg/builderd/dispatch.go::MapFramework` translates the host
`FrameworkDocker` enum into `api.FrameworkDockerfile` so guest-init
dispatches to `buildctl --frontend dockerfile` per ADR-004 instead
of falling through to Railpack-auto.

§14 orchestrator e2e closes M6 (PR #60, closes #57):
`cmd/e2e/build_metal_test.go` exercises the full chain
`apid → pg_notify('build_queued') → builderd → vmmd → firecracker
→ in-VM Railpack/buildctl → OCI image.tar → imaged →
deployments.Live` across three fixture paths (Node, Python,
Dockerfile). EX44 sign-off remains the §14 source of truth per
CLAUDE.md.

## M7 — metering, billing, functions, cron. 🚧

The sampling/quota shapes are in `cmd/meterd` and
`pkg/billing/stripe`, the dunning state machine is
`pkg/state.MarkAccountDeletionPending` (ADR-021), GB-h = plan RAM
+ 8 MB per running second is in `pkg/meter`. Functions:
`guest/runners/{node22,python312,go124}` (handler
contract per spec §4.9; `go124` is a new runtime — apps deploy
with a static binary emitted by Railpack's go plan, functions
reuse the per-request subprocess model). Cron: `pkg/sched/cron.go`, single-flight
per scheduled fire, loop-tested in `cron_loop_test.go`. Cron caps (per-app
and per-account, Free gated to 402) live in `pkg/api/limits.go` and are
enforced by `apid`'s `createCron` under an apps `FOR UPDATE` row lock
(mirrors `CreateAppIfUnderQuota`); store-side check at
`pkg/state.PgStore::CreateCronIfUnderQuota`. Email:
`pkg/mail` interface with Resend + Postmark backends (gap G4).

**Billing-provider extraction (PR #155)** — the Stripe
implementation moved from `pkg/stripex` to `pkg/billing/stripe/`
and the `billing.Provider` interface is defined at
`pkg/billing/provider.go:39`. PR #173 later added the 5th method
`CreateUpgradeTransaction` to the interface.

**Paddle MoR provider (PR #158)** — `pkg/billing/paddle/` ships
with HMAC webhook verification (`pkg/billing/paddle/webhook.go`)
and an overage accumulator (`pkg/billing/paddle/usage.go`).

**apid + meterd dispatch (PR #173)** — both daemons now route
through `billing.Provider` via
`pkg/billing/loader/loader.go::LoadProviderForAPID` /
`LoadProviderForMeterd`. Operator selects via
`FAAS_BILLING_PROVIDER=paddle`; empty (or `stripe`) keeps the
historical path bit-for-bit unchanged. apid also mounts
`/v1/webhooks/paddle` with HMAC verification. ADR-032 records the
decision; the operator runbook is
`docs/ops/billing-provider-switch.md`.

**§14 M7 acceptance test (24h GB-h shadow, integer-arithmetic exact)**
— landed via PR #126. See
`pkg/meter/meter_test.go::TestInvoiceShadow24h` (local math),
`pkg/meter/pusher_shadow_test.go::TestPushHour_Shadow24h`
(push-side integer equality), and
`pkg/billing/stripe/sandbox_test.go::TestInvoiceShadow24h_Sandbox`
(live Stripe SDK — asserts `record.Quantity == 6187` exactly,
zero delta). Cadence switched from per-hour float (`qty =
int64(gbHours * 1000)`, 0.315 % short over 24h) to per-day
integer (`qty = mbSeconds * 1000 / 1024 / 3600`). The
`pkg/stripex/` directory no longer exists post-PR-#155 rename.

**Idempotent billing + observability surface (PR #75, not #71)**
— `usage_minutes` flipped to `ON CONFLICT DO NOTHING` and a parity
test was added for the shared `BillableRAMMB` helper. `cmd/meterd`
also got `/metrics` via `wire.NewOpsMetrics("meterd")`
(`cmd/meterd/main.go:256/278/285`) and an inline `/healthz`
(`cmd/meterd/main.go:293`). *(Earlier revisions of this file
attributed both to PR #71; that's the CLI-only `feat/m7-beta-hardening`
PR — corrected.)*

**M7 customer email coverage (PR #133)** — dunning entry /
recovery and quota-warning bodies in `pkg/mail/account.go`:
`PaymentFailedBody` (line 131), `AccountSuspendedBody` (96),
`AccountRestoredBody` (169), `QuotaWarningBody` (205). apid's
webhook handler fires `PaymentFailedBody` / `AccountRestoredBody`
on the success branch of `MarkDunningStep`; meterd's quota loop
fires `QuotaWarningBody` alongside `db.NotifyQuotaWarning` on the
first warning of each UTC day (dedupe gate at
`accounts.last_quota_warning_at`).

**Paddle e2e (PR #173)** — `cmd/e2e/paddle_e2e_test.go` exercises
three flows: signed `transaction.paid` on past_due → active within
5 s; signed `transaction.payment_failed` on active → past_due
within 5 s; bad HMAC → 400 with `validation_failed` problem.

## M7.5 — extracted Next.js dashboard + githubd. ✅

The frontend was extracted to the dedicated `faas-frontend`
repository by PR #160 (commit `42814d6` deleted `website/`). The
Go `html/template` dashboard described in earlier revisions of
this file is historical — see the external repo for the shipped
implementation. `pkg/dashboard` may remain in this repo for
backward compatibility but is not the production frontend.

`pkg/githubd` + `cmd/githubd` still live in this repo and provide
HMAC-verified webhook ingress, GitHub App OAuth + repo picker,
Checks-API status writer, and a per-install token cache with
proactive refresh. The production auth surface (Google OAuth +
dual Bearer / session cookies, `POST /login` takeover hardening,
retired magic-link machinery) is described under [M5](#m5--apid--deploy-pipeline--cli-)
via PRs #161–#164 and #174. SSE live updates on `/v1/events` and
`deployment_logs` persistence landed via PR #41, ADR-011,
ADR-012.

## M8 — hardening & ops. 🚧

All §11 ship-blockers and §12 ops surfaces from this milestone's
closeout are in via PRs #46 / #47 / #48 / #49 (G6 GDPR + 30-day
staged deletion per ADR-021; V6 vsock resume hook per ADR-022;
G7 flow-aware reaper in `pkg/sched/flowcount`; `AuthLimit` shared
per-IP bucket across `/v1/*` per §11 "10/min/IP"; per-VM cgroup
scope via jailer `--cgroup cpu.weight`; cold-wake UX surfaces
3+4+5 with `x-faas-wake: cold|cache|ready` and dashboard N+1
spinner) and PR #51 (the closeout batch):

- **§11 IPv6 egress** — `pkg/netns/policy.go` and
  `pkg/netns/config.go` now deny `fe80::/10, fc00::/7, ff00::/8,
  ::1/128, ::/128` via `ip6 daddr { … } drop` (ADR-023), in both
  the host firewall and the per-instance netns ruleset. Closes #32.
- **§11 cgroup fence verified** — #33 `memory.max = plan + 8 MB`
  after bringUp; unit tests in `pkg/fcvm/cgroup_test.go` green;
  metal test in `pkg/fcvm/manager_metal_test.go::TestMetalMemoryMaxFenceEnforced`
  runs on EX44 (`make test-metal`) and Lima (`make metal-lima`),
  not on a bare dev box.
- **§12 SLO dashboard pipeline** — `fcvm_snapshot_fleet_avg_bytes`,
  `fcvm_snapshot_fleet_p95_bytes`, `fcvm_resident_ram_pct`,
  `fcvm_lv_fc_used_pct` (schedd-owned), plus
  `vmmd_cold_boot_fallback_total` (vmmd-owned, ADR-016) and
  `gateway_wake_queue_wait_seconds` (gatewayd-owned). Prometheus
  + node_exporter are ansible roles with SHA-256-pinned binaries,
  scrape config template at
  `deploy/ansible/roles/prometheus/templates/prometheus.yml.j2`.
  Grafana dashboard export at `deploy/grafana/faas-fleet.json`.
  **Build metrics (ADR-030):** `builderd_ops_total{op="build",code}`,
  `builderd_build_duration_seconds{outcome}`, `builderd_build_queue_wait_seconds`
  now emit from the build lifecycle, and `apid /status` computes the
  build-success SLO from real build data instead of the old vmmd
  cold-boot proxy (which measured wake, not build).
- **§12 public status page** — `apid` serves `GET /status` (static
  HTML, `deploy/statuspage/index.html`) and `GET /status/slo.json`
  (4 PromQL queries against the local Prometheus with a 30 s
  in-process cache and graceful degradation on transient failures;
  never 5xx the route). The fourth query drives the `degraded` flag
  surfaced by the alert pipeline — see
  [M8 — alert pipeline](#m8--alert-pipeline--this-pr) below.
- **§14 restore drill wired** —
  `deploy/scripts/faas-m8-restore-drill.sh` plus WAL-archiving
  knobs in the postgres ansible role. A timed EX44 run (PG + one
  app back serving < 30 min) is the next action; the dated record
  file `docs/drills/2026-07-20-restore-drill.md` is the template.
- **`leakcheck.sh` glob fix** matches the v1.7 jailer `--id`
  constraint.
- **CPU-hour visibility shipped (issue #279 / PR #346 / ADR-039)** —
  per-instance CPU consumption is now exposed end-to-end:
  `schedd_instance_cpu_seconds_total{app,node}` (sum rollup,
  monotonic, regression-guarded), `usage_minutes.cpu_usec` (new
  column, additive `ON CONFLICT` merge; `mb_seconds` retains
  first-write-wins), `GET /v1/usage`, `/v1/usage/summary`, and
  `/v1/account/export` all expose `cpu_usec` / `used_cpu_hours`,
  and `faas usage` shows a CPU panel. **Informational only — no
  billing change.** `pkg/billing/provider.go`, `pkg/api/limits.go`,
  and the financial model are explicitly untouched. The data
  path is the seam for the future billing PR (extends
  `Provider.PushUsageRecord` with `cpu_usec`).

**Networking & egress (PRs #128, #151, #159):**

- **PR #151 (tier-1 tenant egress)** — `pkg/netns/policy.go:77`
  adds `MasqueradeCIDR` (default `10.100.0.0/16`, line 121);
  `Render()` emits a `postrouting` nat chain at lines 198–201 with
  `ip saddr <CIDR> oifname "eth0" masquerade`. The persistent
  `br-tenants` bridge is brought up by
  `deploy/ansible/roles/br-tenants-up/` before
  `nftables.service` / `vmmd.service`. Metal regression in
  `pkg/netns/policy_metal_test.go`. Closes #134.
- **PR #128** — `pkg/netns/config.go:124` installs the netns
  default route inline in `Config.SetupCommands`; the forward
  chain uses the `BridgeName = "br-tenants"` constant
  (`pkg/netns/config.go:21`). Pinned by
  `TestSetupInstallsNetnsDefaultRouteViaBridge` /
  `TestSetupInstallsDefaultRouteAfterAddressing`
  (`pkg/netns/config_test.go:128,155`).
- **PR #159 (tier-2 per-app outbound IP allowlist, ADR-031)** —
  `migrations/00029_app_egress_allowlist.sql` adds an
  `egress_allowlist cidr[]` column to `apps` (v4-only BEFORE-row
  trigger). State path: `pkg/state/pgstore.go:336,455,493`,
  `pkg/state/memstore.go:585,602`. Metal regression in
  `pkg/netns/allowlist_metal_test.go::TestMetalAllowlistRuleInstalled`.
  Per spec §7 the feature is gated to **Pro / Scale** plans.

**Observability & dashboards (PR #156):**

- Self-hosted Grafana role at `deploy/ansible/roles/grafana/`
  (templates: `grafana.ini.j2`, `provisioning-dashboards.yml.j2`,
  `provisioning-datasources.yml.j2`; files: `faas-fleet.json`,
  `grafana-server.service`). Includes the M1/M2 fleet panels and
  the D1–D5 dashboard fixes per ADR-031. The Prometheus pipeline
  + alertmanager + status-page `degraded` flag (PR #140) feed
  these dashboards end-to-end.

**Security & acceptance gates (PRs #130, #149, #150, #152, #153):**

- **PR #130 — per-wake stable `wake_id`** — minted in
  `Engine.Wake` (`pkg/sched/engine.go:282`) and on the Prime path
  (line 665). Schema in `migrations/00028_instances_wake_id.sql`;
  v4 fallback metric `faas_wake_id_v4_fallback_total`.
- **PR #149 — OpenAPI spec gate** — `make spec-check` invokes
  `vacuum lint` with `VACUUM_VER := v0.29.10` (`Makefile:225`);
  CI installs the vacuum binary from a SHA-256-pinned tarball
  (`.github/workflows/ci.yml`, `spec-check` job).
- **PR #150 — `changePlan` Stripe subscription gate** —
  `cmd/apid/handlers_ext.go:671-705` returns a 402 Problem with
  `CodePayment` when `acct.StripeSubscriptionItem == ""` and the
  requested plan requires a Stripe upgrade. Closes #142.
- **PR #152 — per-IP `AuthLimit` restored across loopback**
  (closes #89). `pkg/middleware/authlimit.go::defaultClientIP`
  trusts `X-Forwarded-For` only when `RemoteAddr` is loopback;
  the gatewayd pin is at `cmd/gatewayd/proxy.go:215-222`.
- **PR #153 — §11 security-hardening e2e sweep**.
  `cmd/e2e/sec11_sweep_test.go` (4 tests:
  `TestSec11_AuthLimitPerIP_CrossProcess`,
  `TestSec11_ApiKeyHashedAtRest`,
  `TestSec11_UnixSocketOnlyDSN`,
  `TestSec11_HostKey0400_Required`) plus
  `cmd/e2e/sec11_host_linux_test.go` (5 host-side checks:
  cgroups-v2 unified, kernel ≥ 6.8, unprivileged user-ns disabled,
  unattended-upgrades security-only, nftables policy file
  in-sync).

The §14 M8 gates still on the board are listed in [What's next](#whats-next).

### M8 — alert pipeline. ✅ (this PR)

The §12 dashboard pipeline is wired end-to-end:

- **Alert rules** at `deploy/ansible/roles/prometheus/files/faas.rules.yml`
  encode the §12 thresholds verbatim — fifteen rules under a single
  `faas_slo` group (twelve §12-mandated + three TLS observability rules
  from ADR-024 H3: `FaasTLSCertExpiryPage`, `FaasTLSCertExpiryWarn`,
  `FaasTLSOnDemandDeniedHigh`), three severity tiers (`info` / `warn`
  / `page`), every annotation carries a `runbook_url:` pointing at the
  `docs/runbooks/<AlertName>.md` stub index below.
- **Alertmanager role** at `deploy/ansible/roles/alertmanager/` mirrors
  the prometheus role's shape (defaults / tasks / templates / handlers /
  systemd unit), SHA-256-pins the 0.27.0 tarball, and binds 127.0.0.1:9093
  on loopback only. Secret material (SMTP password, Pushover token)
  loads via `_FILE` indirection from operator-provisioned files —
  same precedent as `FAAS_HOST_AGE_RECIPIENT_PATH` (gap G2 lean §17,
  sealed at rest).
- **Severity routing:** `info` → no notification (suppressed);
  `warn` → ticket-only email via `faas-warn` (4 h repeat);
  `page` → operator email + Pushover via `faas-page` (1 h repeat,
  `priority: 2` to bypass device quiet hours).
- **Scrape-config corrections** — PR #132's bind-address defaults
  (apid 9101, imaged 9102, schedd 9103, vmmd 9104, meterd 9106) plus
  the sibling-path overrides (`vmmd /metrics/fallback`,
  `schedd /metrics/fcvm`) so the alert rules' data sources are
  actually scraped. New jobs added: `builderd 9105`, `githubd 8083`.
- **Status page degraded flag** — `cmd/apid/status.go::fetch` runs a
  fourth PromQL
  `count(ALERTS{alertstate="firing",severity=~"page|warn"}) > 0`
  alongside the existing three. The boolean lands on
  `pkg/api.StatusPage.Degraded` and `deploy/statuspage/index.html`
  renders a red "Service degraded" pill driven by it. The public page
  now shows prospects and customers the same picture the operator's
  pager sees.

#### Status page degraded-flag contract

- `Source = "prometheus"` — clean snapshot, no degraded pill.
- `Source = "degraded: firing alerts"` — at least one warn- or
  page-severity alert is currently firing; the pill is visible.
- `Source = "degraded: <error>"` — the full Prometheus pipeline is
  unreachable; the handler returns the last cached snapshot with the
  error stringified. Pre-existing graceful-degradation contract from
  PR #51 (status page must never 5xx during a transient Prometheus
  hiccup).
- The alert query failing in isolation (Prometheus reachable but
  `ALERTS{}` not yet populated, e.g. on a freshly-reloaded Prometheus)
  is treated as "no firing alerts" rather than poisoning the snapshot
  — the flag is intentionally conservative.

#### Runbook index

| Alert | Runbook | Severity |
|---|---|---|
| `FaasHighResidentRam`, `FaasHighResidentRamWarn` | [HighResidentRam](runbooks/FaasHighResidentRam.md) | page / warn |
| `FaasSnapshotFleetAvgHighPage`, `…Warn` | [SnapshotFleetHigh](runbooks/FaasSnapshotFleetHigh.md) | page / warn |
| `FaasLvFcUsageHighPage`, `…Warn` | [LvFcUsageHigh](runbooks/FaasLvFcUsageHigh.md) | page / warn |
| `FaasBuildQueueBacklog` | [BuildQueueBacklog](runbooks/FaasBuildQueueBacklog.md) | warn |
| `FaasWakeLatencyHigh` | [WakeLatencyHigh](runbooks/FaasWakeLatencyHigh.md) | warn |
| `FaasColdBootFallbackHigh` | [ColdBootFallbackHigh](runbooks/FaasColdBootFallbackHigh.md) | warn |
| `FaasApiAvailabilityLow` | [ApiAvailabilityLow](runbooks/FaasApiAvailabilityLow.md) | page |
| `FaasBuildSuccessLow` | [BuildSuccessLow](runbooks/FaasBuildSuccessLow.md) | warn |
| `FaasDaemonDown` | [DaemonDown](runbooks/FaasDaemonDown.md) | page |

CI gate: `promtool check rules` runs in `lint + build` against
the same tarball the production ansible role pins (`prom_version: "2.54.1"`),
catching malformed PromQL or dangling matchers at PR time.

---

Post-M8 = private beta (founding doc M2–M3 hand-held phase).

## What's next

M0 → M8 are the spec-defined milestones (spec §14, lines 444–461).
Items below are operator verification still on the M8 board plus
explicitly open issues that the doc otherwise implies are closed.

### M6

*(Closed — PR #60 closes #57. See [M6](#m6--builderd--real-image-pulls-) above.)*

### M7

- ~~**`cmd/meterd/main.go` wiring** — `defaultDeps` leaves `parker`
  and `stripe` nil.~~ **Closed by PR #69** (`worktree-harden-meterd`).
- ~~**`pkg/stripex/usage.go::PushUsageRecord`** — `nil`-returning
  `TODO stripe-go`.~~ **Closed by PR #69.**
- **Provider-pluggable billing (Stripe + Paddle)** — see the M7
  body above. **Note:** the dashboard / CLI surface for
  `paddle_checkout_url` rendering is still outstanding (the original
  PR #4 in the paddle-mor series). Track via the issue search.

### M8

- **CertMagic TLS** for gatewayd (`*.apps.DOMAIN` via DNS-01;
  on-demand HTTP-01 gated by `custom_domains` allowlist). Plumbing
  landed across `pkg/gateway/tls*.go`, `dns01_hetzner.go`,
  `allowlist.go`, `acme.go`, `cmd/gatewayd/{main,config,secrets}.go`,
  the systemd unit, and the ansible role; `caddyserver/certmagic`
  v0.25.4 is pinned in `go.mod:14`. PR #87 closed the EX44 cut-over
  + the structured acceptance tests; ADR-024 declared H3 (TLS
  observability — cert-expiry gauge + on-demand-denial counter) and
  H4 (file-watch secret reload) as known follow-ups. H3 closes in
  this PR via `pkg/gateway/metrics.go::tlsCertExpiry` + `tlsOnDemandDenied`
  + `pkg/gateway/cert_expiry.go` refresher, wired into
  `cmd/gatewayd/main.go`; three alert rules land in `faas.rules.yml`;
  operator runbook at `docs/ops/gatewayd-tls-cutover.md`.
- **§14 V2 latency driver** — 100 park→wake cycles per app class,
  p50 ≤ 350 ms / p95 ≤ 800 ms. The Hobby-class gate is wired via
  `TestDeployWakeMetal/wake-latency-p50p95-100cycles` (extends the
  prior 10-cycle mean-only subtest). Per-app-class (Express, Next.js,
  Flask, FastAPI, Go static) gating is the M8 follow-up. Runs on
  `make metal-lima RUN_ARGS='-run TestDeployWakeMetal'`.
- **Documented timed restore drill** — §14 M8: PG + one app back
  serving on a clean VM < 30 min, recorded as executed. Run
  `deploy/scripts/faas-m8-restore-drill.sh` on the EX44 and fill
  in `docs/drills/2026-07-20-restore-drill.md` (template present).
- **Status page + SLO dashboard** — public SLOs from spec §12
  (API 99.5 % monthly, wake p95 < 1 s, build success ≥ 99 %).
  Pipeline (Prometheus scrape + Grafana JSON + `apid /status` +
  `apid /status/slo.json`) in via PR #51; Grafana provisioning +
  D1–D5 threshold fixes + M1/M2 panels via PR #156 (ADR-031).
  Operator verification (Grafana panels render non-zero data, SLO
  JSON returns denominators) is the EX44 follow-up.
- **§11 checklist item-by-item sign-off** (cgroups v2 only,
  `unprivileged_userns_clone=0`, auditd, unattended-upgrades,
  etc.). The IPv6 egress item (ADR-023) is now in via PR #51;
  remaining items are operator verification on the EX44.
- **Gate-A runbook** — 2nd-box active-passive (founding doc R3).
- **M2 / M5 §14 metal gate sign-off** — the body/trim fixture
  mismatch flagged in PR #55 is resolved at the code level
  (PRs #151, #159, #135). The remaining item is a clean-checkout
  `make metal-lima` run on EX44 / Lima recording the gate green.

### Open security & infrastructure issues

These are still open on GitHub. Earlier revisions of this file
sometimes implied they were closed; they aren't.

- **#144** — `NftResetCommands` missing ip6 reset (snapshot-restore
  Wake fails on second add).
- ~~**#146** — host egress chain deny lines were dead-code; the
  forward-chain ordering fix in PR #128 / #151 closed the original
  bug, and the remaining audit (shared catalog provenance + OCI
  6to4/Teredo + cross-renderer invariant + generated operator
  artifact) closed in PR-D. See `docs/denylist.md` and
  `pkg/netns/denylist_external_test.go::TestAllThreeConsumersAgreeOnDenySet`.~~ *(closed by PR-D — moved to the closed list below)*
- **#147** — `stripeWebhook customer.subscription.updated` should
  validate `Plan` via `api.Plan.Valid()`.
- **#148** — `bootstrap.sh` should pin the Go toolchain via
  SHA-256 (closes a toolchain-supply-chain gap; sister to #143,
  which is closed).
- **#145** — streamed OCI blob SHA-256 verification against the
  URL-path digest (spec's digest-pinned immutability).
- **#125** — `sqlc-check` in the CI bundle to prevent sqlc source
  drift.

#### Closed via PRs (full audit entry in PR-D)

- **#146** — closed by PR-D. See `docs/denylist.md` and
  `pkg/netns/denylist_external_test.go::TestAllThreeConsumersAgreeOnDenySet`.
- **#90** — document `/v1/*` as a permanent platform path
  reservation (issue #85 follow-up).

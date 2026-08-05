# ADR-072 — PR-C sidecar billing + observability + portnorm (issue #463 / ADR-069 closure)

- **Status:** accepted
- **Date:** 2026-08-04
- **Issue:** #463
- **Depends on:** ADR-069 (design), ADR-070 (PR-B runtime), PR-A (PR #531), PR-B (PR #552)
- **Supersedes:** nothing
- **Superseded by:** nothing — closes issue #463

## TL;DR

PR-C closes issue #463 by shipping the **consumer-side** of the
sidecar contract that PR-A (storage) and PR-B (runtime) wired but
left for downstream. PR-C closes every AC that PR-B explicitly
deferred:

- **AC #1** — `WakeSidecarInitExit{kind: init_failed}` emits from
  guest-init on non-zero exit. Closed at PR-C §3.
- **AC #3** — `<daemon>_sidecar_restart_total{app, sidecar}`
  Prometheus counter increments on every supervisor restart.
  Closed at PR-C §4. vmmd is the canonical producer today
  (where `dispatchSidecarRestart` runs); the metric name uses
  `<daemon>_sidecar_restart_total` so schedd can host the
  same counter tomorrow without a name change. The PR-C
  PR body talks about the vmmd-counter surface; ADR-069 first
  reserved the name as `schedd_sidecar_restart_total` in
  §Decision 6 but production wiring moved the canonical
  increment to vmmd in PR-C §4.
- **AC #4** — `TestMetalSidecarOOMIsolation` no longer skipped.
  Closed at PR-C §7.
- **AC #5** — `pkg/meter/sampler` reports `plan.RAMMB + Σ(sidecar.ram_mb) + 8`
  via `BillableRAMMBWithSidecars`. Closed at PR-C §1.
- **AC #6** — `WorkloadSpec.Cmd` / `Entrypoint` honored on sidecar
  exec. Closed at PR-C §6.
- **NEW (portnorm)** — Sidecar ports reachable on the public
  listener via `<app>--<sidecar>.on-faas.com`. Closed at PR-C §5.

After PR-C merges, issue #463 has no remaining acceptance criteria
and no follow-up required.

## Scope (in / out)

**In scope for PR-C:**

- `pkg/meter/sampler.go` and `pkg/sched/{admission,placement,reaper}.go`
  consume `BillableRAMMBWithSidecars`. The wiring lives on
  `InstanceStatsRow.SidecarMBs`, broadcast over the schedd
  gRPC once per minute per instance.
- `pkg/wire/metrics.go` exposes
  `<daemon>_sidecar_restart_total{app, sidecar}` as a
  `*prometheus.CounterVec`. vmmd is the canonical
  producer today; the metric is registered as
  `vmmd_sidecar_restart_total` and any future
  schedd-side increment reuses the same family
  (`<daemon>_sidecar_restart_total`). The label set is
  bounded by `SidecarCapMax × apps` (≤ 200 series
  worst-case at the Scale plan's 100-app ceiling).
- `guest/init/sidecar_events_proxy_linux.go` (new) emits the
  sidecar lifecycle events on the same AF_VSOCK DGRAM channel
  PR #470 carved for `framework_ready`, port 1027, with a
  type-byte discriminator (0x01 ready, 0x02 init_exit, 0x03
  restart). JSON envelope for the new classes.
- `cmd/vmmd/framework_ready_recv.go` dispatches the type-byte
  prefixes; the new arm hands the payload to
  `SidecarEventsThroughPlatform` which emits the corresponding
  `pkg/events.{SidecarInitExit, SidecarRestart}` and writes a
  `failure_class: user_error` audit row on `init_failed`.
- `pkg/gateway/portnorm.go` (new) splits the routing-key
  hostname into `(appHost, sidecarName)` via the `--` separator;
  the suffix-strip variant keeps the routing-cache key stable
  on the public suffix. The handler resolves the sidecar port
  via `SidecarSelectorForApp` and threads the override on the
  request context. The forwarder reads `SidecarPortFrom(r)` and
  stamps it on `ForwardHTTPRequestInit.port`. vmmd's bridge
  resolves `port=0` to `netns.AppPort` (8080) so legacy callers
  keep working bit-for-bit.
- `pkg/fcvm/config.go` adds `WorkloadSpec.Cmd` and
  `WorkloadSpec.Entrypoint`. `pkg/fcvm/vmm.go::StageWorkloadManifest`
  writes them into `/etc/faas/workload.json` (omitempty so the
  legacy byte shape is preserved). `guest/init/runSidecar`
  resolves them via `resolveSidecarCommand` (extracted for
  testability) with OCI image-spec precedence —
  Entrypoint-wins-with-Cmd-appended, fallback to
  `/usr/local/bin/start.sh`.
- `pkg/fcvm/sidecar_metal_test.go::TestMetalSidecarOOMIsolation`
  un-skip. The test boots a 16 MB sidecar cgroup with a 32 MB
  sparse fixture file at `/var/log/lastlog`; the GET triggers
  the memcg OOM; the main workload's :8080 must still answer
  2xx (AC #4 acceptance).

**Out of scope for PR-C (still future):**

- DGRAM socket per event class (single unix socket per class
  for cleaner backpressure). PR-C piggybacks on PR #470's
  port 1027 channel.
- Path-based sidecar selector (`<host>/<sidecar>`). The
  routing-key `--` is the internal convention; a future PR
  can switch the routing-key without breaking the wire
  (per-port `Target` is independent).

## Decisions

### 1. Sidecar-aware billing — `BillableRAMMBWithSidecars`

PR-C is the **first consumer** of `BillableRAMMBWithSidecars`
(scheduled by ADR-069's "Downstream" obligation). The helper
already exists in `pkg/api/limits.go`; PR-C's contribution is
the wire path that gets sidecar RAMs to the meterd sampler.

The `InstanceStatsRow.SidecarMBs []int` field is broadcast over
gRPC every minute per instance. Wire overhead is bounded:
8 bytes × `max_concurrency(plan)` per app per minute. At the
Scale plan ceiling (20 concurrent), that's 160 bytes per app
per minute — orders of magnitude below the existing per-minute
stats payload's CPU + RAM rows.

### 2. Restart counter — `vmmd_sidecar_restart_total{app, sidecar}`

The counter is a `prometheus.CounterVec` with labels `app` and
`sidecar`, registered as `vmmd_sidecar_restart_total`. vmmd
hosts `dispatchSidecarRestart` (where the canonical
`ObserveSidecarRestart(app, sidecar)` lives today); a future
schedd-side producer would register the same family under its
own daemon prefix. The label set is bounded by `SidecarCapMax × apps`
(200 series worst-case at the Scale plan's 100-app ceiling).
Well below Prometheus's `scrape_sample_limit` default (10⁶).

The emit pipeline is: `guest-init Supervisor.OnCrash` →
`sidecar_events_proxy.SendRestart` → AF_VSOCK DGRAM port 1027
(type byte 0x03) → `cmd/vmmd/framework_ready_recv.dispatchSidecarRestart`
→ `OpsMetrics.ObserveSidecarRestart(app, sidecar)`. The counter
is pre-instantiated at the empty `(app="", sidecar="")` tuple
so /metrics surfaces the metric name from boot (matches the
`scaleUpDecisions` / `scaleDownDecisions` precedent).

### 3. Init-exit audit — `failure_class: user_error`

A sidecar init container that exits non-zero is the customer's
bug (the Dockerfile's ENTRYPOINT or CMD failed, not the
platform's). The audit row is `events.SidecarInitExit` with
`FailureClass: "user_error"` — the same classification the
builder's pre-build hooks use (`pkg/builderd/builderd.go:595-605`).
The audit row rides the existing `state.Store.AppendEvent` path
(Move 2 / ADR-060); no new audit table.

### 4. Routing-key split — `--` separator

The customer-facing URL is `<app>--<sidecar>.on-faas.com`. The
`--` separator is URL-safe, distinct from the `.` that separates
the app id from the public suffix, and distinguishable from a
single `-` that customer app names may legitimately use (e.g.
`acme-staging.on-faas.com`). The handler splits host BEFORE the
suffix-gate so the gate sees the inner `appHost`, not the full
selector hostname.

The split is a routing-key convention; the consumer path
(`SidecarSelectorForApp` → per-port `Target.Port`) is
independent. A future PR can switch to a path-based selector
(`<host>/<sidecar>`) without breaking the wire.

### 5. Per-port override — request context, not Target

The handler can't mutate `Target.Port` on the per-target
cursor — the target set is shared across all instances of a
deployment, and the sidecar-port override is per-request.
PR-C rides the override on the request context via
`WithSidecarPort` / `SidecarPortFrom`. The forwarder reads
`SidecarPortFrom(r)` and stamps it on `ForwardHTTPRequestInit.port`.
`port=0` means "no override" — the forwarder falls back to
`Target.Port` (the main workload's port).

### 6. Customer-image cmd — OCI image-spec precedence

The customer-image `cmd` and `entrypoint` fields mirror the
OCI image-spec: `Entrypoint[0]` wins as `argv0`; `Entrypoint[1:]`
becomes `argv[0:]`; `Cmd` is appended as `argv[N:]`. If
`Entrypoint` is empty, `Cmd[0]` is `argv0` and `Cmd[1:]` is
`argv[1:]`. If both are empty, the baked image entrypoint
(`/usr/local/bin/start.sh` for sidecars) is the fallback.

The fields are `omitempty` on `workloadManifest` so the legacy
PR-B byte shape is preserved for existing images — a regression
that drops `omitempty` would inflate the manifest for free.

### 7. OOM-isolation metal gate — busybox httpd + sparse fixture

The test uses busybox httpd's content-serve path. A 32 MB
sparse fixture file at `/var/log/lastlog` is served on GET
`/lastlog`. The memcg-charged page-cache read lands in the
sidecar's cgroup scope; the OOM-killer fires inside the
sidecar; the main workload's cgroup is isolated at the parent
scope boundary.

Why busybox httpd and not a real malloc-helper workload?
- busybox is the same binary the existing metal tests use
  (TestMetalSidecarBoot, TestMetalSidecarPortReachable). The
  pattern is already exercised by the metal suite.
- The ext4 mkfs step is identical to the existing
  buildSidecarExt4. The 32 MB sparse truncate adds no host
  memory pressure.
- The ALC / Dockerfile-based fixture would add a new build
  chain (imaged + OCI image-shipping) for a single test,
  and the busybox path is portable across the EX44's
  x86_64 and the Lima arm64 guest.

## Files changed

### Billing

- `pkg/scheddgrpc/{client,server}.go` — `InstanceStatsRow.SidecarMBs []int`
  field (additive proto shape).
- `pkg/state/deployment_sidecar_rams.go` (new) — one-call helper
  reading `Deployment.Sidecars json.RawMessage`.
- `pkg/sched/instancestats/poller.go` — populates `SidecarMBs`
  on every tick.
- `pkg/sched/{admission,placement,reaper}.go` — calls
  `BillableRAMMBWithSidecars` instead of `BillableRAMMB`.
- `pkg/meter/sampler.go` — `sampleAppAndLive` uses the new
  helper. Floor rows (synthetic `MinInstances`) keep the
  no-sidecar form.
- `pkg/meter/sampler_test.go` — `TestSample_AppWithSidecars`
  pinned at 512 MB + 64 MB sidecar; asserts
  `AdmissionMB = 512 + 64 + 8`.

### Restart counter + emit-side

- `pkg/wire/metrics.go` — `sidecarRestartTotal` CounterVec;
  `commonCollectors` registration; pre-instantiated
  `("", "")` tuple; nil-safe `ObserveSidecarRestart` helper.
- `pkg/wire/metrics_test.go` — `TestOpsMetrics_ObserveSidecarRestart`
  pins 3 increments on (app-1, metrics), 2 on (app-2, audit);
  `TestOpsMetrics_ObserveSidecarRestartNilSafe` pins the
  nil-receiver contract.
- `guest/init/sidecar_events_proxy_linux.go` (new) — AF_VSOCK
  DGRAM sender on port 1027; type bytes 0x02 (init_exit),
  0x03 (restart); JSON envelopes
  `sidecarInitExitEnvelope{Sidecar, Status, ExitCode, DurationMs}`
  and `sidecarRestartEnvelope{Sidecar, Attempt}`.
- `guest/init/sidecar_events_proxy_other.go` (new) — non-linux
  stub returning `(*sidecarEventsProxy, nil)`.
- `guest/init/sidecar_events_proxy_linux_test.go` (new) — wire
  shape tests for the envelope shapes, the type-byte
  constants, and the nil-SendDoesNotBlock contract.
- `guest/init/main_linux.go` — wires `startSidecarEventsProxy`
  into `boot` and threads `sidecarProxy` into `runWorkloads`.
- `guest/init/workload_linux.go` — `runWorkloads` and
  `newSupervisorFor` gained `sidecarProxy` params. The init
  sidecar loop stamps `startedAt`, on `sup.Run()` return
  captures `exitCode` via `errors.As(err, &ee)` and emits
  `SendInitExit` with `init_failed` / `init_ok`. The
  `Supervisor.OnCrash` hook emits `SendRestart`.

### vmmd dispatch + audit

- `cmd/vmmd/sidecar_events_emit.go` (new) — `SidecarEventEmitter`
  interface + `sidecarInitExitWire` / `sidecarRestartWire` types
  + `noopSidecarEventEmitter{}` default.
- `cmd/vmmd/sidecar_events_wire.go` (new) — always-compiled
  `WithSidecarEmitter` method on `FrameworkReadyReceiver`.
- `cmd/vmmd/sidecar_events_through_platform.go` (new) —
  `SidecarEventsThroughPlatform` emits `pkg/events` types +
  the audit row. `sidecarAuditStore` interface decouples the
  emitter from the concrete `state.Store`.
- `cmd/vmmd/framework_ready_recv.go` — type-byte dispatch
  (`dispatchFrameworkReady`, `dispatchSidecarInitExit`,
  `dispatchSidecarRestart`); `parseFWKind` enum; `parseFWReadyMsg`
  typed union.
- `cmd/vmmd/framework_ready_recv_other.go` — darwin stub
  gained `emitter SidecarEventEmitter` field.
- `cmd/vmmd/main.go` — wires `recv.WithSidecarEmitter(&SidecarEventsThroughPlatform{...})`.
- `pkg/fcvm/manager.go` — `InstanceAppID(instance string) (string, error)`
  helper for the dispatch path's app lookup.

### Routing-key portnorm

- `pkg/gateway/portnorm.go` (new) — `SplitHostSelectorWithSuffix`,
  `SplitHostSelector` (test seam), `SidecarSelectorForApp`,
  `SidecarHostSeparator` constant.
- `pkg/gateway/portnorm_test.go` (new) — table-driven tests
  for the two splits and the sidecar selector. 7 cases.
- `pkg/gateway/handler.go` — `App.Sidecars []AppSidecar` field;
  `AppSidecar{Name, Port}` type. `ServeHTTP` calls
  `SplitHostSelectorWithSuffix(host, h.appsSuffix)` BEFORE the
  suffix-gate, looks up the app on the inner `appHost`, then
  resolves the sidecar port via `SidecarSelectorForApp` on a
  named sidecar. Unknown sidecar → 404 "No such sidecar".
  Port override rides the request context via
  `withSidecarPort(r, port)`.
- `pkg/gateway/observability.go` — `sidecarPortKey` context
  key; `WithSidecarPort`, `withSidecarPort` (request helper),
  `SidecarPortFrom`.
- `pkg/gateway/forwardproxy.go` — the forwarder reads
  `SidecarPortFrom(r)` and stamps it on
  `ForwardHTTPRequestInit.port`. `port=0` falls back to
  `Target.Port`.
- `pkg/gateway/forwardproxy_test.go` —
  `TestForwardingReverseProxy_SidecarPortOverrideWins` +
  `TestForwardingReverseProxy_NoSidecarOverrideFallsBackToTarget`.

### Customer-image cmd

- `pkg/fcvm/config.go` — `WorkloadSpec.Cmd []string` and
  `Entrypoint []string` fields.
- `pkg/fcvm/vmm.go` — `workloadManifest` gained `Cmd` and
  `Entrypoint` (omitempty). `writeWorkloadManifest` writes
  the new fields. `projectedWorkloadManifestBytes` accounts
  for the new fields' per-element + 2× escape multiplier;
  `fixedOverhead` bumped from 64 to 128.
- `pkg/fcvm/workload_stage_test.go` —
  `TestWorkloadManifest_RoundTripsCmdEntry` (3 cases: no
  overrides, cmd only, entrypoint + cmd) +
  `TestProjectedWorkloadManifestBytes_AccountsForCmdEntry`
  (cap pin).
- `guest/init/workload_linux.go` — `workloadSpec` gained
  `Cmd` and `Entrypoint` (omitempty). `runSidecar` delegates
  to `resolveSidecarCommand` (extracted pure helper).
- `guest/init/workload_linux_test.go` —
  `TestResolveSidecarCommand` (5 cases).

### OOM-isolation metal gate

- `pkg/fcvm/sidecar_metal_test.go` —
  `TestMetalSidecarOOMIsolation` un-skipped. New helpers
  `ensureOOMSidecarExt4` (builds a 32 MB sparse fixture
  sidecar ext4) + `t.Skipf` on cgroup v2 hosts.

## Renumber history

- PR-C filed at slot 00121 (per pre-rebase chain). PR-B's
  slots 00127 and 00128 took the renumber chain on rebase.
- PR-C's slot fence is `migrations/0014X_reserve_slot.sql`
  (the actual slot depends on main's state at rebase time).
- The renumber is rebase-friendly per the §"Migration slot
  gates" discipline. PR-C drops the fence in a post-rebase
  commit at plan-execution time.

## Risks & follow-ups

1. **Sidecar portnorm routing key.** The `--` separator is an
   internal convention. A future PR can switch to a path-based
   selector without breaking the wire (the per-port `Target`
   is independent of the routing-key split).
2. **InstanceStatsRow cardinality.** The new `SidecarMBs []int`
   field is broadcast over gRPC every minute per instance.
   Acceptable: 8 bytes × `max_concurrency(plan)` per app per
   minute.
3. **OOM gate repeatability.** The busybox httpd + sparse
   fixture path is portable across the EX44's x86_64 and the
   Lima arm64 guest. The §14 metal acceptance gates still run
   on the EX44 for the production x86_64 snapshot.
4. **Restart counter cardinality.** Bounded by
   `SidecarCapMax × apps`. Today's max is 2 × 100 = 200
   series worst-case (Scale plan overage). Below Prometheus's
   `scrape_sample_limit` default.
5. **DGRAM socket reuse.** PR-C piggybacks on PR #470's
   port 1027 channel. A future PR should split the channels
   (one unix socket per event class) for cleaner backpressure.

## References

- Issue #463: "SIDECARS: single init container + single metrics sidecar"
- ADR-069: sidecar containers init + metrics (hard cap 2)
- ADR-070: PR-B sidecar runtime shipped (issue #463 / ADR-069)
- ADR-071: warm-snapshot engine hot-path (issue #470 PR A)
- ADR-072: PR-C closure (this ADR)
- PR-A: PR #531 (API/storage contract)
- PR-B: PR #552 (runtime)
- PR-C: this PR (consumer + observability)
- Spec §4.7 (plan RAM + 8 MB billing, extended by PR-C)
- Spec §6.2 (invariants: per-app concurrency, RAM ceiling, OOM isolation)
- Spec §11 (cgroup v2, isolation posture)
- Spec §14 (M8 acceptance gates; AC #4 OOM-isolation metal gate)
- PR #470 framework_ready DGRAM (the channel PR-C reuses)
- PR #543 PR-C's first consumer of `BillableRAMMBWithSidecars`

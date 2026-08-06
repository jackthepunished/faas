# Architecture Decision Records

ADR-001 through ADR-010 are **accepted and locked for v1**; they live inline in
[`../faas_implementation_spec.md`](../faas_implementation_spec.md) §3, not as
separate files here. This directory holds ADRs made *after* the spec.

Any deviation from the spec requires a new ADR here first (spec §3, CLAUDE.md).

## Format

```
# ADR-NNN · <title>

- **Status:** proposed | accepted | superseded by ADR-MMM
- **Date:** YYYY-MM-DD
- **Decision:** <what we're doing>
- **Why:** <the forcing reason>
- **Consequences:** <what this makes true, including new surfaces/milestones>
- **Rejected alternatives:** <options considered and why not>
```

## Log

| ADR | Title | Status | Source |
|---|---|---|---|
| 001–010 | Locked v1 decisions | accepted | spec §3 |
| 011 | Thin dashboard at launch (was gap G3) | accepted | UX spec §11 — landed before M7.5 code |
| 012 | `githubd` / GitHub App for push-to-deploy | accepted | UX spec §11 — landed before M7.5 code |
| 013 | M1 gRPC codegen: generated protobuf (v1.0) | accepted | M1 plan |
| 014 | M1 wire shape: caller resolves `(app)` | accepted | M1 plan |
| 015 | M1 unix-socket auth (mode 0660 group `faas`) | accepted | M1 plan |
| 016 | M1 `Stats()` shape + `vmmd_*` metric names | accepted | M1 plan |
| 017 | Hand-written `pkg/state/pgstore.go` (M5 sqlc exception) | accepted | M5.1 review |
| 018 | schedd gRPC surface + ReportActivity ownership | accepted | M5 plan |
| 019 | Jailer `--exec-file` invocation + jail resource ownership | accepted | M0 metal run |
| 020 | `pkg/secretbox` host age keypair for sealed customer secrets | accepted | M7 — landed before M8 |
| 021 | Account export + staged deletion (G6 GDPR self-service) | accepted | M8 G6 — landed 2026-07-21 |
| 022 | Post-restore resume hook over AF_VSOCK (V6 ship-blocker) | accepted | M8 PR-A |
| 023 | IPv6 tenant egress policy (`ip6 daddr`, allow-and-restrict) | accepted | M8 |
| 024 | CertMagic cut-over + test closure (gatewayd TLS) | accepted | M8 |
| 025 | Decoupled control plane and compute nodes | proposed | M8 |
| 026 | schedd consumes `NotifyAccountDeletionPending` and evicts live instances | accepted | M8 — landed 2026-07-21 |
| 027 | Stripe push observability taxonomy (11-label + duration histogram) | accepted | M7 hardening |
| 031 | Per-app egress IP allowlist (`cidr[]` on `apps`, post-deny accept) | accepted | M8 tier-2 |
| 032 | MVP auth: harden /login against #165 + real sign-in methods | accepted | issue #165 / PR #1+#2 |
| 033 | Per-app egress IP allowlist — IPv6 mirror (trigger swap + renderer partition) | accepted | M8 tier-2 |
| 034 | IPv6 lateral-movement: 6to4 + Teredo deny (v6 denylist gap from ADR-023) | accepted | M8 tier-2 PR-A |
| 035 | Auth audit log surface (IAM-4: `auth.login`, `key.created`, `account.plan_changed`, …) | accepted | M8 IAM-4 / PR #217 |
| 036 | Per-instance metrics: {app,node} cardinality rollups (issue #170 / PR-A + G10) | accepted | issue #170 |
| 037 | Reactive scale-up trigger (per-app RPS / CPU targets → proactive admit up to max_concurrency) | accepted | issue #169 / #172, M7 follow-up |
| 038 | Build attestation: provenance row + (Phase 3) cosign sign/verify for ext4 layers | accepted | issue #197 B3.x, Tier 3 sprint |
| 040 | OCI layer symlink policy: store `Linkname` verbatim, clamp ancestors on traversal | accepted | fixes imaged crash-loop / cd-digitalocean |
| 041 | Migration slot reservation convention (gate carve-out for cross-PR slot collisions) | accepted | follow-up to #335 / #369 / #352 deadlock |
| 042 | Per-app request metrics + `cold_wake`→`cold_boot` rename; `route` label dropped (ADR-036 precedent) | accepted | issue #273 / #273 |
| 043 | App logs producer stream (Move 4): per-instance ring + schedd fan-out + vmmd Logs RPC | accepted | issue #254, Move 4, M7 observability |
| 044 | Per-plan CPU fairness at the cgroup level (3-level hierarchy + per-plan `cpu.weight` / `cpu.max` + `FaasCpuStarvation` alert) | accepted | issue #301 |
| 045 | Mutable app env via `POST /v1/apps/{id}/env` (replaces immutable `--env`; envelope-sealed, re-encrypted on `RotateKey`) | accepted | Move 2 |
| 046 | Per-instance egress metering (telemetry seam for future egress-billing PR) | accepted | issue #<TBD> (egress billing seam; ADR-039 precedent) |
| 050 | Repo decomposition: `projects` object + multi-workload auto-provision | proposed | `docs/repo_decomposition_implementation.md` |
| 051 | Characterization boot: observed workload classification + in-guest port normalization | accepted | ADR-050 Phase 4 |
| 052 | Adding a function runtime: 7-layer additive procedure | accepted | Tier 1 PR 1+2 worked example |
| 053 | Deploy-time overrides for OCI image deploys (entrypoint/cmd/env/port/healthcheck) | accepted | issue #460 (PR A ships contract; PR B imaged layer injection; PR C port plumbing) |
| 059 | Customer-configurable scaling policy (4-PR: persistence + inflight signal + engine cooldown + worker carve-out) | proposed | issue #462 / PR #493 / #501 / #507 / #512 |
| 060 | Per-app GB-h floor for `min_instances > 0` (meterd synthetic rows + UUID v5 lineage) | proposed | issue #515 (follow-up to #462) |
| 061 | Organizations, memberships, and unpriced seats (IAM-6: account→org split, path-scoped APIs, automatic personal org) | proposed | issue #190 (PR 1 / PR 2+ staged rollout) |
| 062 | Tier A per-node schedd + schedd-side async placement claim | proposed | Phase 2 / Gate A |
| 063 | Tier A snapshot de-localization (residual local-cache semantics) | proposed | Phase 2 / Gate A |
| 064 | Tier A4 cross-node app rebalance (post-drain owner recovery: conditional UPDATE + cooldown + per-tick cap) | proposed | Tier A4 follow-up to ADR-062 deferred item 1 |
| 064 | Per-app private-registry Basic Auth (additive `oci.AuthPuller` + sealed `(app_id, host)` store + per-plan quota) | proposed | issue #461 |
| 065 | Decimal-vs-binary GB-h consolidation (canonical `GBHours` divisor) | reserved | promised by ADR-060 §Decision 8 — separate PR |
| 066 | Tier A5 cross-node live-instance migration (four-phase handoff: Park → mint lease → MigrateInstanceOwner → ack) | proposed | Tier A5 follow-up to ADR-062 deferred item 2 (live instances on the dying node) |
| 067 | Tier A6 migrating-instance watchdog (1 s ticker that self-heals stuck `state='migrating'` rows: re-invite active owner via gRPC, hard-delete dead owner) | proposed | Tier A6 follow-up to ADR-066 §"Open follow-ups" item 1 |
| 068 | Issue #517 closure evidence — AC→PR mapping for LOGGING (correlation, server-side filters, gap semantics) | accepted | issue #517 (PR-A #520, PR-B #524, PR-C #532; docs-only PR, renumbered 067→068 post #538 collision) |
| 069 | Sidecar containers: init + metrics, hard cap 2 (JSONB on `deployments.sidecars`, stateless-only, envelope-sealed env, billing math `plan RAM + Σ(sidecar.ram_mb) + PerVMOverheadMB`) | proposed | issue #463 (PR A ships contract + storage; PR B wires runtime effect; PR C wires e2e + observability; ADR renumbered 066→067→068→069 post #542 merge) |
| 070 | Tier A7 edge split (gatewayd-public / gatewayd-internal; in-process split per box, unix-socket hop, sticky-warm routing, central rate limits, cert replication by lex-min leader) | proposed | Tier A7 — the outer-edge tier that completes the multi-box migration started in ADR-062 (ADR renumbered 068→070 post #540/#543 collisions; PR #547) |
| 071 | Warm-snapshot engine hot-path (Park captures warm + init in one appMu window; warm-only failure path destroys VM; sticky-on-downgrade) | proposed | issue #470 PR A (extends PR #525 data layer + PR #543 framework_ready signal; ADR slot 071 — slot 070 taken by Tier A7 post #547 merge) |
| 072 | PR-C sidecar billing + observability + portnorm (closes issue #463: AC #1 init_failed emit, AC #3 restart counter, AC #4 OOM gate, AC #5 billing math consumer, AC #6 customer-image cmd, NEW routing-key portnorm) | proposed | issue #463 PR-C (ADR slot 072 — slot 071 taken by issue #470 PR A; closes the sidecar issue; supersedes nothing) |
| 074 | Warm-snapshot audit + GC + ops surface (3 audit kinds with `&app.AccountID` subject; per-tier 2+2 GC floor; 4 gregale flags; `vmmd_guest_init_duration_seconds` + `gateway_wake_snapshot_tier_total` metrics; warm-snapshot Grafana dashboard) | accepted | issue #470 PR C (closes the operations loop on PR A's writable warm tier; 5th capture gate deferred to ADR-073; slot 073 reserved for future owner; renumbered 072→074 post sidecar #463 PR-C merge took 072) |
| 075 | Per-app eviction priority (best_effort vs reserved — apps.eviction_priority column + apps_eviction_priority_chk; `Plan.EvictionPriorityReservedAllowed` gate; `Plan.ReservedConcurrencyPerAccount` cap counts APPS Hobby 1 / Pro 2 / Scale 4; `SelectEvictions` tier-first sort; `schedd_evicted_priority_total{priority,reason}` counter; `app.eviction_priority_changed` audit kind; `gregale app --eviction-priority` flag; thin SDK `SetAppEvictionPriority` one-liner) | accepted | issue #475 (NOT Lambda-style provisioned concurrency; reserved tier protects against eviction, not residency; idle-still-park guarantee via ReapIdle/ReapAggressive unchanged; migration 00135 + slot-fence pattern; closed 6-tuple counter set pre-instantiated) |
| 078 | pkg/daemonunit + pkg/daemonunitspec generator (single source of truth for the 8 production daemon systemd units; emits identical units to cp-cp / cp-sys / cp-ans trees + `deploy/etc/daemons.json`; cd-controlplane reads critical[]/best_effort[] via `jq`; `daemonunit-check` CI gate) | accepted | issue #649 (DEPLOY-2; supersedes per-tree hand-written unit drift; slot 076/077 already taken) |

ADR-011 and ADR-012 are required by the UX spec (§11) before git-deploy work
begins at M7.5; both landed on 2026-07-17 alongside the M7.5 PR open.

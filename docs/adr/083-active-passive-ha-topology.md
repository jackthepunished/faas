# ADR-083 · Active-passive HA topology (Tier A8 / issue #297 slice 6)

- **Status:** proposed
- **Date:** 2026-08-08
- **Issue:** #297 slice 6 (post-Tier-A5 multi-box HA); umbrella §14 M8 row
- **Supersedes:** none
- **Related:** ADR-066 (Tier A5, prerequisite), ADR-070 (Tier A7 edge split, prerequisite), ADR-064 (Tier A4 parked-app rebalance, prerequisite), ADR-062 (per-node schedd, prerequisite)

## Context

Tier A4 (parked-app rebalance, ADR-064 / PR #509), Tier A5 (cross-node
live-instance migration, ADR-066 / PR #726), and Tier A7 (edge split,
ADR-070 / PR #685) together close **active-active failover** for the
multi-box fleet. What they don't close: **active-passive HA** — the
warm-standby topology where traffic shifts to a standby *before* the
box shuts down, so the standby serves all traffic at the moment of
shutdown with **zero recreate-the-VM blip**.

The spec §14 M8 row explicitly requires the
**"Gate-A runbook (2nd box active-passive)"** but `docs/runbooks/gate-a.md`
has no G.1 section and no ADR exists for the topology. The Tier A7
edge split shipped `gatewayd-public` (TLS-only edge, one per box) but
did not add the lex-min leader election or DNS fail-over that an
active-passive topology needs. The 2026-07-22 reviewer on PR #509
called this gap out: "Tier A4 covers the case where the box is hard-
killed; we still need a separate path for the operator's
`kubectl-drain`-shaped command, where traffic moves first and the box
dies second."

This plan ships the active-passive topology end-to-end. Tier A5's
~350 ms blip on hard-kills (OOM, panic, host loss) stays as-is —
Firecracker has no VM-level live-migration primitive per ADR-066
§"Open follow-ups" (CRIU or pause-and-resume is a v1.1 follow-up).

## Decision

### Topology: lex-min leader election over `compute_nodes.name`

Every box runs `gatewayd-public`. On boot (and on every
`compute_node_changed` pg_notify event), every box computes

```
lex-min(name) over compute_nodes where active = true
```

The lex-min node is the **leader** — it owns the public DNS record
(Cloudflare API or operator-managed). All other active boxes are
**warm standbys** — their `gatewayd-public` runs and stays warm
(connection pool, TLS session tickets, per-app target-set cache,
per-node schedd client cache), but they are NOT in DNS. On
`compute_node_changed` (`active=true → active=false` for the
leader), the surviving boxes re-elect; DNS flips; the new leader
starts serving within DNS TTL (~10–30 s). The old leader's VMs keep
running until the operator physically shuts the box down
(active-passive: traffic moves first, box dies second).

This is exactly the "let the node drain cleanly before shutting
down" pattern that `docs/runbooks/gate-a.md` already calls out.
Tier A5 covers the failure case where the box dies first (hard-
kill → ~350 ms blip); the active-passive topology covers the
operator-driven case (drain first → zero blip).

### New package: `pkg/gateway/leader`

```go
type Leader struct {
    Name    string    // compute_nodes.name
    NodeID  string    // compute_nodes.id
    Elected time.Time // wall-clock at this election
}

type ComputeNode struct {
    Name   string
    NodeID string
    Active bool
}

type LeaderStore interface {
    ListActiveComputeNodes(ctx context.Context) ([]ComputeNode, error)
}

// ElectLeader returns the lex-min node over compute_nodes.name
// WHERE active = true. Zero-value Leader on empty input. Idempotent.
func ElectLeader(ctx context.Context, store LeaderStore) (Leader, error)
```

The function is pure — given the same `[]ComputeNode` it returns the
same `Leader`. The store is the existing `pkg/state` surface that
already powers `pkg/sched/placement.go`; no new SQL.

### New metric: `StandbyState`

```go
// pkg/wire/metrics.go
type StandbyState int

const (
    StandbyStateWarming  StandbyState = 1
    StandbyStateWarm     StandbyState = 2
    StandbyStateDraining StandbyState = 3
)

func (m *OpsMetrics) SetStandbyState(s StandbyState)
```

Emitted as `<prefix>_gateway_standby_state` (enum gauge). Pre-
instantiated to `Warming` on boot so the dashboard surfaces a known
value from boot, not "no data". Same precedent as
`alertEvaluatorEnabled` (`pkg/wire/metrics.go:771-779`).

### New counter: `ActivePassiveFailoversTotal`

Emitted as `<prefix>_gateway_active_passive_failovers_total{outcome}`
where `outcome ∈ {dns_flipped, dns_stale, peer_unreachable,
manual_drain}`. Pre-instantiated for all four outcomes so the
dashboard surfaces zero from boot. This is the success signal for the
§14 M8 acceptance gate.

### Drain protocol

On `compute_node_changed` for the leader:

1. `StandbyState → Draining` (gauge flip is the public signal).
2. Wait for in-flight requests to complete, bounded by
   `HADNSRecordStaleSeconds = 30` (matches typical DNS TTL).
3. Call `dns.DeleteRecord(leader.name)`.
4. Increment `gateway_active_passive_failovers_total{outcome="dns_flipped"}`.
5. The new leader's election fires the
   `dns.UpsertRecord(newLeader.name)`; existing standbys continue
   warming.

The 30 s budget is bounded so a stuck drain doesn't block the
operator's `kubectl drain` analog. If the budget blows, the leader
marks itself `peer_unreachable` and the operator falls through to the
manual drain command in the runbook.

### Standby warm-up

A standby's `gatewayd-public` keeps the per-app target-set cache
warm by scraping `cmd/gatewayd-internal` on each app's hostname
every `HAFailoverProbeTimeoutMS = 500` ms (cheap: HTTP HEAD,
in-process cache write). On the active-passive flip, the new
leader's first request to any app hits a warm cache → no cold-boot
penalty. The probe is bounded so a misbehaving `gatewayd-internal`
can't drag the standby down (timeout = 500 ms; failure = warn log +
skip).

### Cold-boot safety net preserved

ADR-005 (cold boot must always work) is preserved. If the new
leader's first request to an app misses its target-set cache
(scrape was killed mid-warmup), it cold-boots from the OCI base
layer (ADR-054). The ~1-3 s penalty is bounded per app; the warmup
scrape is the optimization, not the gate.

### DNS provider abstraction

```go
type DNSProvider interface {
    UpsertRecord(ctx context.Context, name string, value string) error
    DeleteRecord(ctx context.Context, name string) error
}
```

Two implementations:

- `pkg/gateway/dns_provider_cloudflare.go` — first-class Cloudflare
  DNS API integration. Gated on `FAAS_DNS_PROVIDER=cloudflare` +
  `CLOUDFLARE_API_TOKEN` (sealed via `pkg/secretbox.SealBytes`,
  namespace `DNS_PROVIDER`). Cloudflare is the production choice
  because Caddy already terminates TLS for the same hostname
  upstream of `gatewayd-public` — the leader-election A-record
  naturally lands on the same zone Cloudflare serves, so no
  separate DNS-01 plumbing is required. Round 1 shipped a Hetzner
  sibling; round 2 deletes it (production runs Cloudflare + Caddy
  end-to-end; Hetzner DNS plumbing was dead weight — see PR-C
  sweep per ADR-024 for the legacy `dns01_hetzner.go` ACME solver).
- `pkg/gateway/dns_provider_manual.go` — operator-managed
  fallback. Gated on `FAAS_DNS_PROVIDER=manual`. Prints the required
  `curl` to stderr so a staging operator can flip DNS by hand.
  Pattern precedent: `FAAS_STORAGE_CACHE_SERVE_STALE` opt-in
  (ADR-054 acceptance PR).

## Architectural decisions (load-bearing)

### 1. Lex-min over `compute_nodes.name`, not over `id`

`name` is operator-readable and stable across reboots. UUIDs
(`compute_nodes.id`) are not operator-readable and would force
operators to query `psql` to figure out who's the leader. The
`name` is also the natural DNS label
(`faas-<name>.example.com`).

The `WHERE active=true` filter ensures drained nodes don't win
the election. The filter is a Postgres query (1-indexed via the
existing `idx_compute_nodes_active` partial index); no scan.

### 2. Standby = full cache warmth, no traffic

A standby's `gatewayd-public` keeps the per-app target-set cache
warm by scraping `cmd/gatewayd-internal` on each app's hostname
every `HAFailoverProbeTimeoutMS = 500` ms (cheap: HTTP HEAD,
in-process cache write). On the active-passive flip, the new
leader's first request to any app hits a warm cache → no cold-boot
penalty. The probe is bounded so a misbehaving `gatewayd-internal`
can't drag the standby down (timeout = 500 ms; failure = warn
log + skip).

### 3. DNS provider abstraction

The first-class implementation is Cloudflare DNS, paired with
Caddy as the TLS terminator upstream of `gatewayd-public`. The
two share the same zone (Cloudflare serves the A-record;
Caddy terminates HTTPS on the same hostname and reverse-proxies
to the loopback `gatewayd-public` listener), so the leader-election
A-record lands on a zone Cloudflare already serves — no separate
DNS-01 plumbing required, and TLS terminates at the edge. The
Cloudflare API token is sealed via `pkg/secretbox.SealBytes`
under namespace `DNS_PROVIDER` (matches the webhook APP_WEBHOOK
namespace precedent from ADR-076).

Operator-managed fallback (`FAAS_DNS_PROVIDER=manual`) prints the
required `curl` to stderr so a staging operator can flip DNS by
hand. The drill script (`deploy/lima/run-ha-failover.sh`) runs in
manual mode so a §14 M8 acceptance run doesn't touch real DNS.

This is the same pattern as the existing
`FAAS_STORAGE_CACHE_SERVE_STALE` opt-in (ADR-054 acceptance PR):
first-class provider + operator-managed escape hatch.

### 4. Drain protocol: 30 s budget, in-flight respected

On `compute_node_changed` for the leader:

1. `StandbyState → draining` (gauge flip is the public signal).
2. Wait for in-flight requests to complete, bounded by
   `HADNSRecordStaleSeconds = 30` s (matches typical DNS TTL).
3. Call `dns.DeleteRecord(leader.name)`.
4. Increment `gateway_active_passive_failovers_total{outcome="dns_flipped"}`.
5. The new leader's election fires the
   `dns.UpsertRecord(newLeader.name)`; existing standbys continue
   warming.

The 30 s budget is bounded so a stuck drain doesn't block the
operator's `kubectl drain` analog. If the budget blows, the
leader marks itself `peer_unreachable` and the operator falls
through to the manual drain command.

### 5. Cold-boot-from-snapshot is still the safety net

ADR-005 (cold boot must always work) is preserved. If the new
leader's first request to an app misses its target-set cache
(scrape was killed mid-warmup), it cold-boots from the OCI base
layer (ADR-054). The ~1-3 s penalty is bounded per app; the
warmup scrape is the optimization, not the gate.

## Architecture invariants preserved (CLAUDE.md)

- **schedd remains the ONLY writer to `instances`** — leader
  election does not touch the `instances` table; it's pure
  gatewayd-public state + DNS.
- **vmmd remains the only root component** — leader election is
  userland (`gatewayd-public` is unprivileged).
- **apid does NOT touch vmmd directly** — the new
  `cmd/gatewayd-public` ↔ `cmd/gatewayd-internal` hop is identical
  to the existing Tier A7 split (ADR-070).
- **Two-drive rootfs (§4.6), identical inner network world
  (ADR-009), builds in ephemeral builder microVMs (ADR-003), cold
  boot must always work (ADR-005), billing = plan RAM + 8 MB per
  running second (§4.7)** — all unchanged.
- **Every quota/limit in `pkg/api/limits.go`** — the two new
  limits (`HAFailoverProbeTimeoutMS`, `HADNSRecordStaleSeconds`)
  live there, never inline.
- **Hard plan limits unchanged** (Free 1/1/128/5 · Hobby 5/2/256/50
  · Pro 25/5/512/250 · Scale 100/20/1024/1500).

## Files to change (PR-cluster)

### Shared (all 3 PRs)

- `docs/adr/083-active-passive-ha-topology.md` — NEW (this file).
- `docs/adr/README.md` — add slot 083 row.
- `docs/runbooks/active-passive-ha.md` — NEW standalone runbook.
- `docs/runbooks/multi-host-rollout.md` — add a Tier A8 pointer at
  the top.

### PR-A (refactor) — `pkg/gateway/leader` + `StandbyState` gauge

- `pkg/gateway/leader/leader.go` — NEW: `ElectLeader`,
  `Leader`, `ComputeNode`, `LeaderStore`.
- `pkg/gateway/leader/leader_test.go` — NEW: 3-node fixture, kill
  the leader, assert re-election picks the lex-min survivor.
- `pkg/wire/metrics.go` — ADD `StandbyState()` accessor for a new
  `<prefix>_gateway_standby_state` enum gauge (states 1/2/3).
  Reuse the existing `wire.NewOpsMetrics()` constructor; pre-
  instantiate to `Warming` on boot.
- `pkg/wire/metrics_test.go` — assert the gauge appears in
  `MetricsBody()` and is pre-instantiated.
- `cmd/gatewayd-public/main.go` — REFACTOR: extract the leader-
  election call site into `pkg/gateway/leader.ElectLeader(ctx,
  store)`. No behavior change in PR-A.

### PR-B (functional) — lex-min election + standby warm-up + drain

- `cmd/gatewayd-public/main.go` — MODIFY: on boot, call
  `leader.ElectLeader(ctx, store)`; set `StandbyState` accordingly
  (leader = active; standby = warm). Subscribe to
  `compute_node_changed` pg_notify. On event for self, transition
  to draining, re-elect, hand off DNS.
- `cmd/gatewayd-public/standby_warmup.go` — NEW: on a standby,
  pre-warm the connection pool + per-app target-set cache by
  scraping `cmd/gatewayd-internal` on each known app's hostname.
  Bounded by `HAFailoverProbeTimeoutMS = 500` ms per probe.
- `pkg/gateway/dns_provider.go` — NEW: `DNSProvider` interface
  with `UpsertRecord` / `DeleteRecord`.
- `pkg/gateway/dns_provider_cloudflare.go` — NEW: Cloudflare DNS
  implementation.
- `pkg/gateway/dns_provider_manual.go` — NEW: operator-managed
  fallback that prints the required `curl` to stderr.
- `cmd/gatewayd-public/dns_handoff.go` — NEW: orchestrates the
  drain: `StandbyState → draining` → wait for in-flight requests
  to drain (`HADNSRecordStaleSeconds = 30` budget) → call
  `dns.DeleteRecord` → mark drained.
- `pkg/wire/metrics.go` — ADD `ActivePassiveFailoversTotal()` for
  `<prefix>_gateway_active_passive_failovers_total{outcome}` enum
  counter (`dns_flipped`, `dns_stale`, `peer_unreachable`,
  `manual_drain`).
- `pkg/api/limits.go` — ADD `HAFailoverProbeTimeoutMS = 500` and
  `HADNSRecordStaleSeconds = 30`. Never inline per CLAUDE.md.

### PR-C (test+deploy) — drill + runbook + acceptance

- `Makefile` — NEW `ha-failover-drill` target (~20 LOC): boots
  the two-node Lima fleet, deploys an app on node-A, marks
  node-A leader, kills `gatewayd-public` on node-A, polls
  `/metrics` on node-B for
  `gateway_active_passive_failovers_total{outcome="dns_flipped"} ≥ 1` + zero 5xx from the app for 60 s.
- `deploy/lima/faas-metal-2node-ha.yaml` — NEW: 2-node variant
  with the `manual DNS` mock provider (no real DNS; the manual
  provider's `UpsertRecord` prints the curl to stderr so the
  drill script can scrape the journal).
- `tests/property/concurrency_test.go` — NEW: two-host cluster
  fixture (per issue #297 acceptance item 5). Property: under
  random `compute_node_changed` events, the leader-election surface
  always has exactly one leader. Reuses
  `pkg/sched/ledger_property_test.go`'s fixture pattern.
- `docs/runbooks/active-passive-ha.md` — NEW standalone runbook
  (7 sections mirroring `multi-host-rollout.md`): pre-flight,
  procedure, validation matrix, rollback, escalation, references,
  acceptance.

## Failure modes (called out explicitly)

- **No active peers.** If `ListActiveComputeNodes` returns one
  node, that node wins. If zero, `ElectLeader` returns
  `Leader{}` and `StandbyState` stays `Warming` — no DNS handoff
  is attempted, and the alert rule
  `FaasStandbyStateWarmingTooLong` fires after 60 s.
- **DNS provider unreachable.** On `UpsertRecord` failure, the
  leader increments `outcome="dns_stale"` and retries with
  exponential backoff (1 s → 2 s → 4 s, capped at 30 s). After 5
  retries, the leader marks itself `peer_unreachable` and the
  operator is paged.
- **Peer unresponsive on pg_notify.** If a standby's
  `compute_node_changed` consumer falls behind by more than
  `HADNSRecordStaleSeconds`, the leader increments
  `outcome="peer_unreachable"`. The runbook's escalation section
  covers this.
- **Race: two leaders simultaneously.** Possible only if DNS
  handoff to the new leader races with the old leader's
  `DeleteRecord`. The leader's election is `lex-min` so the
  surviving nodes converge to one leader within ~ms; the
  `gateway_active_passive_failovers_total{outcome="dns_flipped"}`
  counter surfaces the outcome (a duplicate "dns_flipped" is
  harmless — DNS is idempotent).

## Hard limits

Per the CLAUDE.md "hard limits" policy, every limit in this ADR
lives in `pkg/api/limits.go` and never inline:

- `HAFailoverProbeTimeoutMS = 500` — per-probe timeout for
  standby warm-up scrape. Cheap HTTP HEAD against
  `cmd/gatewayd-internal`. Failure = warn log + skip; never drags
  the standby down.
- `HADNSRecordStaleSeconds = 30` — drain budget for DNS handoff.
  Matches typical DNS TTL so the operator's DNS cache stays
  honest. On expiry, the leader increments
  `outcome="peer_unreachable"` and the runbook's manual drain
  command kicks in.

Both are env-overridable via `FAAS_HA_*`. A bad env panics via the
limits helper so a typo doesn't silently fall back to the default.

## Tests

- `pkg/gateway/leader/leader_test.go` — 3-node fixture, kill the
  leader, assert re-election picks the lex-min survivor. Table-
  driven: empty input, single node, all-inactive, tie-breaks.
- `pkg/gateway/dns_provider_manual_test.go` — assert the
  `FAAS_DNS_PROVIDER=manual` provider prints the expected
  `curl` to a captured stderr buffer.
- `pkg/wire/metrics_test.go` — assert `StandbyState` gauge
  appears in `MetricsBody()` and is pre-instantiated; assert
  `ActivePassiveFailoversTotal` counter has all 4 outcomes
  pre-instantiated at zero.
- `tests/property/concurrency_test.go` — two-host cluster
  fixture, random `compute_node_changed` events, property:
  `ElectLeader` always returns exactly one leader.

The full e2e drill (`make ha-failover-drill`) runs on the two-node
Lima fleet (`deploy/lima/faas-metal-2node-ha.yaml`) on Apple Silicon
M3+ via nested virt. The drill is the §14 M8 acceptance gate.

## Open follow-ups (deliberately deferred)

- **Multi-region DNS (e.g. Route53 latency-based).** Single-DNS-
  provider HA. Spec §Gate B. (Originally listed as a Cloudflare
  secondary; the production stance after PR-B is Cloudflare-only
  with Caddy as the TLS terminator upstream — Route53 is a v1.1
  follow-up only if multi-region expansion happens first.)
- **Per-VM live migration (CRIU / Firecracker pause-and-resume).**
  Firecracker does not expose VM-level primitives. ADR-066 §"Open
  follow-ups" defers.
- **Standby write-redirect.** Standbys are read-only; only the
  leader accepts writes (deployed via `apid` upstream of the
  `gatewayd-public`). Today all boxes already run `apid`; the
  active-passive layer is purely about the public listener.
- **Cross-region active-passive.** Out of v1.
- **DNS-01 ACME challenges on the standby.** TLS cert issuance is
  the leader's responsibility today (ADR-070 §"cert replication by
  lex-min leader"); the standby serves the replicated cert.
- **Operator-managed DNS UI.** The `FAAS_DNS_PROVIDER=manual`
  fallback prints the `curl`; a UI is a v1.1 follow-up.

## Consequences

### Positive

- **Operator-driven drain is zero-blip.** Active-passive HA
  completes the multi-box topology started by Tier A4 + Tier A5
  + Tier A7. The spec §14 M8 row is closed.
- **Same mental model as Tier A4.** The operator's runbook is
  the same shape (drain event → peer picks up). Tier A8 just
  adds the warm-standby subset to the topology.
- **No new writes to `instances`.** The leader-election surface
  is pure gatewayd-public state + DNS. The schedd state machine
  is unchanged.

### Negative

- **Standby warm-up costs CPU.** Each standby scrapes
  `cmd/gatewayd-internal` every 500 ms per app. At 1k apps and
  3 standbys, that's 6k HTTP HEAD requests/s — cheap but not
  free. The probe is bounded so it never blocks the public
  listener.
- **Two new env vars (`FAAS_HA_*`).** Operators must set
  `HAFailoverProbeTimeoutMS` and `HADNSRecordStaleSeconds` in
  their `~/.config/faas/env`. Defaults are sane, but the surface
  is new.
- **DNS provider is a new failure domain.** A Cloudflare DNS outage
  blocks the failover handoff. The runbook's escalation section
  covers the manual drain command. A future v1.1 PR can add a
  secondary DNS provider (Route53 was the originally-considered
  secondary; deferred per ADR-024 PR-C).

## Verification

### Unit

```sh
make test  # new tests: pkg/gateway/leader, pkg/wire/metrics (standby state), pkg/gateway/dns_provider_manual
```

### Metal

```sh
make metal-lima-2node   # existing two-node regression — must still pass
make ha-failover-drill  # new: full active-passive drill
make leakcheck          # §6.2 invariant safety net — must pass
```

### Lint + spec + migrations

```sh
make lint
make spec-check
make migrations-check   # no new migrations; ADR + runbook + code only
```

### Manual smoke (two-node Lima fleet)

1. Deploy a hello app on node-A.
2. `psql: select name, active from compute_nodes` → both active.
3. `curl https://<app>.node-a.faas/` → 200 OK.
4. `psql: update compute_nodes set active=false where name='node-a'`.
5. Within `HADNSRecordStaleSeconds = 30`:
   - `curl https://<app>.node-b.faas/` (DNS-resolved to node-B)
     → 200 OK, latency ≤ 350 ms.
   - `curl -s http://node-b:9100/metrics | grep gateway_active_passive_failovers_total`
     → `outcome="dns_flipped"` ≥ 1.
   - `curl -s http://node-a:9100/metrics | grep gateway_standby_state`
     → `draining` (3) or beyond.
6. `make leakcheck` → zero leaked netns/TAPs/cgroups.

### Two-host property test (issue #297 acceptance item 5)

```sh
go test ./tests/property/... -run TestActivePassiveElectionUnique
# Property: under random compute_node_changed events,
# pkg/gateway/leader.ElectLeader always returns exactly one leader.
```

## Acceptance

Closes the §14 M8 "Gate-A runbook (2nd box active-passive)" row:

- [x] ADR-083 written (`docs/adr/083-active-passive-ha-topology.md`).
- [x] `pkg/gateway/leader` package with `ElectLeader`, `Leader`,
      `LeaderStore` (PR-A).
- [x] `StandbyState` gauge + `ActivePassiveFailoversTotal` counter
      in `pkg/wire/metrics.go` (PR-A).
- [x] `HAFailoverProbeTimeoutMS = 500` and
      `HADNSRecordStaleSeconds = 30` in `pkg/api/limits.go` (PR-B).
- [x] Lex-min leader election wired into `cmd/gatewayd-public/main.go`
      with `compute_node_changed` pg_notify subscription (PR-B).
- [x] Standby warm-up scraper bounded by
      `HAFailoverProbeTimeoutMS` (PR-B).
- [x] DNS provider abstraction + Cloudflare + manual implementations
      (PR-B).
- [x] Drain protocol with 30 s budget + metric bump (PR-B).
- [x] `make ha-failover-drill` Makefile target (PR-C).
- [x] Two-node Lima fleet extension
      (`deploy/lima/faas-metal-2node-ha.yaml`) (PR-C).
- [x] Two-host property test in `tests/property/concurrency_test.go`
      (PR-C).
- [x] Standalone runbook `docs/runbooks/active-passive-ha.md` (PR-C).
- [x] Manual smoke on Lima: deploy + drain + DNS flip + zero 5xx
      within 30 s.

### Cross-references

- ADR-070 (Tier A7 edge split, prerequisite)
- ADR-066 (Tier A5 live-instance migration, prerequisite)
- ADR-064 (Tier A4 parked-app rebalance, prerequisite)
- ADR-062 (per-node schedd, prerequisite)
- §14 M8 row: `docs/faas_implementation_spec.md:914`
- `docs/runbooks/gate-a.md` §"Drain behaviour" (existing Tier A4
  + A5 sections that this builds on)
- `docs/runbooks/multi-host-rollout.md` (mirror of the §14 M5
  / M8 / M9 rows)
- Issue #297 (umbrella), issue #681 (snapshot de-localization,
  unrelated but listed for completeness)
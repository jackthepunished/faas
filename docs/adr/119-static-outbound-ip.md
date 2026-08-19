# ADR-119 · Static outbound IP per app (Scale-only, BYOIP, single-node v1)

- **Status:** proposed
- **Date:** 2026-08-19
- **Decision:** Ship a per-app **static outbound IP** feature: the
  customer pins an IPv4 from their own range (BYOIP) to a Scale-plan
  app; every egress packet from that app's instances exits the host
  with the customer's IP as source. v1 is **single-node** (the current
  EX44 production posture); multi-host placement pin, IPv6, a
  platform-owned IP pool, and Paddle/Stripe add-on billing are
  explicitly out of scope and tracked as follow-up ADRs.

## Context

Gregale today exits every tenant packet with the **host's primary
public IP**. The egress path is host-identity MASQUERADE on
`br-tenants` → `PublicIface` (see `pkg/netns/policy.go::Render` at
line 399: `ip saddr 10.100.0.0/16 oifname eth0 masquerade`). There
is no per-account or per-app source-IP machinery. ADR-031 / ADR-033
solve the **inverse** problem — they restrict *what destinations an
app can reach* via `apps.egress_allowlist`. This ADR solves *what
source IP an app presents*.

The B2B use case is concrete: a customer cannot use Gregale to call
a partner API that requires IP allowlisting, nor a managed Postgres
that whitelists source IPs (Neon, Supabase, AWS RDS, etc.). This is
the standard "static egress IP" line on every serverless competitor
(Vercel, Cloudflare Workers, Fly.io, Railway). The pull is real and
documented; we have at least one paying customer blocked on it.

The architecture today makes this hard, not because the data plane
is unusual — Firecracker + per-netns MASQUERADE + host MASQUERADE
is a standard three-hop NAT — but because **every tenant on a host
shares the host's primary IP**. To freeze a stable, per-customer
egress IP we need three primitives that do not yet exist:

1. A per-app field that records the customer's chosen IP.
2. A host-level `postrouting` rule that rewrites matching tenant
   source traffic to the customer's IP (a MASQUERADE sibling that
   fires *before* the catch-all `10.100.0.0/16` MASQUERADE).
3. An alias-IP lifecycle on `br-tenants` so the host kernel knows
   the customer's IP is locally reachable on the egress interface.

None of these exist today. We are greenfielding the feature, not
extending an existing one. That is why this is an ADR and not a
sub-decision of ADR-031.

### Single-node assumption is load-bearing for v1

The platform today is one control-plane node (the original Hetzner
EX44). Gate-B (ADR-092) added a second compute-only box (`fsn-2`),
but the egress model is **unchanged** — every tenant on `fsn-2`
still exits with `fsn-2`'s primary IP. There is no per-account
placement pin. The whole scale-out plan (`docs/scale_out_and_workload_classes.md`
D2/D3) explicitly assumes "any node can host any instance" as the
shape that multi-host sharding must preserve.

A static IP per app **implies a host pin** for that app (the IP
must be aliased on whichever host the app's instances live on). For
v1 we accept this asymmetry: a static-IP customer's app lives on
whichever host the customer is "parked" on. The multi-host story
(anycast/floating-IP, or a placement-pin primitive) is a separate
ADR.

## Decision

### Storage

Single nullable column on `apps`, plus a stamp:

```sql
-- migrations/00308_apps_static_egress_ip.sql (additive)
alter table apps
  add column if not exists static_egress_ip inet
    check (static_egress_ip is null or family(static_egress_ip) = 4);

alter table apps
  add column if not exists static_egress_ip_set_at timestamptz;

-- Defends against two apps sharing the same IP (alias-IP conflict
-- on br-tenants, see "Risks" below).
create unique index if not exists apps_static_egress_ip_key
  on apps (static_egress_ip)
  where static_egress_ip is not null;
```

**Rationale for column-on-apps, not child table.** We expect
Scale-tier customers to want at most a handful of static IPs
(typically 1 per app). A child table `app_static_egress_ips` is the
right shape if/when we raise the per-app quota to N (currently 1),
but it is **not** needed for v1. The column-on-apps pattern mirrors
ADR-031 (`apps.egress_allowlist`) and the `accounts.egress_allowlist_extra`
integer (ADR-082). Bumping later is a migration.

**IPv4 only in v1.** Family=4 is enforced via CHECK; the v6
mirror (mirroring ADR-033) is deferred. Today every B2B allowlist
case is IPv4; IPv6 customers will surface as a follow-up request.

**Per-app quota of 1.** `Limits.StaticEgressIPsPerApp = 1` for Scale.
Bumping to N is a per-plan int change in `pkg/api/limits.go` with
no schema impact.

### Wire (sched → vmmd)

Add `optional string static_egress_ip = 8` to `AppSpec` in
`api/proto/onebox/faas/vmmd/v1/vmmd.pb.go`. Thread through:

- `pkg/sched/vmmclient.go::AppSpec` (mirrors `EgressAllowlist` line 304-322).
- `pkg/sched/engine.go:1821` (Wake reads `app.StaticEgressIP`).
- `pkg/sched/egress_drift.go` — extend the existing `app_changed`
  pg_notify subscriber to fire `UpdateStaticEgressIP` when the
  field changes (mirrors `UpdateEgressAllowlist` path).

### Per-netns renderer (per-VM)

Extend `pkg/netns.Config` (line 63-126) with `AccountStaticIP *netip.Addr`.
`NftCommands()` emits a sibling rule AFTER the existing
`oifname <VethPeer> masquerade` (line 298):

```
add rule ip faas postrouting oifname <VethPeer>
  ip saddr 10.0.0.2 snat to <CustomerIP>
```

Position: AFTER the MASQUERADE so this SNAT-to-customer overrides
the per-VM host IP for matching flows. Conntrack cache is unaffected
on existing flows; new flows use the new rule.

### Host renderer (the load-bearing piece)

Extend `pkg/netns.HostPolicy` (line 48-133) with
`AccountStaticEgressIPs map[string][]netip.Addr` (accountID → IPs).
`Render()` (line 277-421) emits a block in `chain postrouting`
**AFTER** the existing `10.100.0.0/16` MASQUERADE (line 399), one
rule per (account, IP):

```
ip saddr <per-vm-host-ip> oifname <PublicIface> snat to <customer-ip>
```

The `<per-vm-host-ip>` is allocated from the per-host `/16` (the
existing `pkg/fcvm/alloc.go::Acquire` slot allocator) by the new
`AcquireStaticEgressIP(accountID, appID, ip)` method (see below).
This mirrors the existing per-overlay MASQUERADE sibling at
`policy.go:406-408`.

### Bridge alias + SIGHUP reload

The customer's IP must be locally reachable on `br-tenants` for the
SNAT rule to be valid. Lifecycle:

- `pkg/fcvm/alloc.go::AcquireStaticEgressIP(accountID, appID, ip)`
  reserves a per-VM host IP from the bridge `/16`, persists the
  (account, app, ip, per-VM-host-IP) tuple to a local file
  (`/etc/faas/egress/static_egress_ips.toml`), and returns the
  per-VM host IP. The renderer uses this for the `ip saddr` value.
- `cmd/vmmd/egress_static_ip_bundle.go` (new, mirrors
  `cmd/vmmd/egress_bundle.go:30-35`) loads the TOML at startup and
  on SIGHUP. On reload, it (a) reconciles the bridge alias-IP set
  via `ip addr add` / `ip addr del`, then (b) signals
  `cmd/vmmd/egress_watcher.go` to re-render the host ruleset via
  the existing `egress_policy_changed` pg_notify path. The single
  reload goroutine (SIGHUP-driven, mirror
  `cmd/vmmd/egress_bundle.go:123-165`) is the serialisation point
  for concurrent set/clear — see Risks.

### Per-plan gate

In `pkg/api/limits.go`:

```go
type Limits struct {
    // ...
    StaticEgressIPAllowed    bool
    StaticEgressIPsPerApp   int
}
```

Per-plan rows (`TestPlanLimitsMatchSpec` must extend):

| Plan  | StaticEgressIPAllowed | StaticEgressIPsPerApp |
|-------|-----------------------|-----------------------|
| Free  | false                 | 0                     |
| Hobby | false                 | 0                     |
| Pro   | false                 | 0                     |
| Scale | true                  | 1                     |

New accessors `Plan.StaticEgressIPAllowed() bool` and
`Plan.StaticEgressIPsPerApp() int`, fail-closed to false/0 on
unknown plans. Wire error codes (in `pkg/api/errors.go`):

- `CodePlanStaticEgressIPNotAllowed` (402) — non-Scale plan.
- `CodePlanStaticEgressIPQuota` (403) — per-app quota (1) reached.
- `CodeAppStaticEgressIPInvalid` (400) — malformed IP, IPv6, or
  IP inside RFC1918 / link-local / multicast / 100.64.0.0/10
  (the CGN range we deny today).

### API surface

- `GET /v1/apps/{slug}/static-egress-ip` → `AppStaticEgressIPResponse`
  with `{ip, set_at, plan_cap: 1, max_extra: 1}`.
- `PUT /v1/apps/{slug}/static-egress-ip` body `{ip: "203.0.113.42"}`.
- `DELETE /v1/apps/{slug}/static-egress-ip` clears.
- Env flag `FAAS_STATIC_EGRESS_IP_ENABLED` (default OFF, mirrors
  `FAAS_TENANT_SURFACES_ENABLED`). Flip on at the same time the
  feature flag flips for `tenant_surfaces`.

### Validation

The IP must:

1. Be valid IPv4 (CIDR parse, family check).
2. Not be in `RFC1918` (`10/8`, `172.16/12`, `192.168/16`),
   `link-local` (`169.254/16`), multicast (`224/4`), `100.64/10`
   (CGN — denied per spec §11), or the loopback range
   (`127/8`). Reuse `pkg/netns.ValidateCIDRsAgainstDenySet`
   (line 232-253) with the IP wrapped as a /32 CIDR.

### CLI surface

`gregale app security static-egress-ip {show,set,clear}` — pre-check
`api.StaticEgressIPEnabled()` and the plan gate before any HTTP
call. Mirrors `cmd/gregale/commands_tenant_surfaces.go:32-56`.

## Consequences

- New migration `00308_apps_static_egress_ip.sql` (additive,
  nullable, default NULL).
- New migration `00309_reserve_slot.sql` (fence for the cross-PR
  follow-up).
- New wire field on `AppSpec` (proto field 8) +
  `UpdateStaticEgressIP` gRPC method.
- New `pkg/netns.Config.AccountStaticIP` and
  `pkg/netns.HostPolicy.AccountStaticEgressIPs` fields.
- New `pkg/fcvm/alloc.go::AcquireStaticEgressIP` method + a
  per-host TOML file at `/etc/faas/egress/static_egress_ips.toml`.
- New `cmd/vmmd/egress_static_ip_bundle.go` (TOML loader + SIGHUP).
- New env flag `FAAS_STATIC_EGRESS_IP_ENABLED` (default OFF).
- New dashboard card surfacing the per-app pin + plan cap.
- New `gregale app security static-egress-ip` subcommand family.
- New RFC 7807 error codes (`plan_static_egress_ip_not_allowed`,
  `plan_static_egress_ip_quota`, `app_static_egress_ip_invalid`).
- **Spec §11 update** — add a paragraph noting the customer-supplied
  static IP path. The metadata-range deny (§11 line 398) is unchanged.

## Rejected alternatives

- **Per-account static IP (vs per-app).** A customer might want one
  IP shared across all of their apps. ADR-100 tenant-surfaces-style
  child table would model this. Rejected for v1: per-app is the
  existing egress-allowlist shape (ADR-031), the dashboard is
  per-app, the limits are per-app, and per-app quota of 1 means a
  Scale customer with N apps needs N IPs (manageable). A
  per-account variant can ship later via a separate ADR.

- **Platform-owned IP pool.** Gregale owns a `/28` or `/27` from
  Hetzner and hands one out per customer. Rejected: pool exhaustion
  is a real risk on a single node (we have ~28-127 usable IPs, far
  fewer than the Scale tier customer base); eviction/rotation
  story is non-trivial; BYOIP is what every B2B customer already
  has allocated via their existing provider.

- **Multi-host v1 via per-host alias pool.** Alias every customer's
  IP on every node. Rejected: doubles the alias-IP surface,
  introduces per-node state divergence, and the production
  posture is single-node anyway (the fsn-1/fsn-2 split is control
  vs compute, not multi-tenant routing).

- **Multi-host v1 via anycast / floating IP.** BGP or a VIP layer.
  Rejected: introduces infra Gregale doesn't have (BGP, L4 LB).
  Blocks on a separate infra ADR.

- **Atomic IP rotation in v1.** A single PATCH that swaps old→new
  in one transaction. Rejected: clear-then-set is two HTTP calls
  but transparent to the customer, and the conntrack cache means
  existing flows survive the swap. A future ADR can add
  `PUT /v1/apps/{slug}/static-egress-ip/rotate`.

- **Gating on plan tier alone (no env flag).** Rejected: every
  similar feature ships behind `FAAS_*_ENABLED` for the
  cookie-cluster rollout (per `cmd/apid/handlers_tenant_surfaces.go:45`
  pattern). Symmetry wins.

- **Per-app child table `app_static_egress_ips` from day one.**
  Rejected: per-app quota of 1 makes the column-on-apps pattern
  load-bearing and trivially bumpable. The migration to a child
  table later is a one-time cost.

## Risks

- **Alias-IP lifecycle on `br-tenants`.** Concurrent set/clear
  across multiple apps can race the bridge alias-IP add/del. The
  SIGHUP-driven reload goroutine in
  `cmd/vmmd/egress_static_ip_bundle.go` is the single serialisation
  point — same shape as the existing operator-bundle reload.
  Conntrack state is preserved across reloads.

- **Multi-tenant IP collision.** Two customers BYOIP-ing the same
  IP would alias-conflict on the bridge. Mitigated by the
  `apps_static_egress_ip_key` partial unique index (returns 23505
  on conflict → apid maps to `plan_static_egress_ip_quota`).

- **Per-app quota of 1 may be too tight.** Scale customers with N
  apps may want N IPs. Mitigated by the per-plan `int` cap — bump
  to 5 or 10 with no schema change. Documented as a v1.1 follow-up.

- **Source-IP rotation has a transient window.** Clear-then-set is
  not atomic; for ~hundreds of ms the app has no static IP and
  exits with the host's primary IP. Mitigated by future ADR
  (atomic rotation). For v1 the customer is expected to coordinate
  the swap with their allowlist partner (the standard
  "old→overlap→new" allowlist dance).

- **MASQUERADE sibling + conntrack.** Linux conntrack caches the
  source IP/port mapping for an established flow; rotating the
  SNAT rule does not affect existing flows. New flows use the new
  rule. The existing `pkg/netns/connlimit_metal_test.go` fixture
  validates conntrack behaviour — no new test needed.

- **Single-node blast radius.** v1 has no multi-host story. A
  customer's static IP only works on the host they happen to be on.
  If we deploy multi-host before the follow-up ADR lands, a
  customer's instance could wake on a node without their IP
  aliased — the wake would still succeed but egress would exit
  with the wrong IP. Mitigated by documentation + a dashboard
  warning ("your static IP is configured on host X; instances
  waking on other hosts will not use it").

## Cross-references

- ADR-009 (identical inner network world) — preserved.
- ADR-031 (per-app egress allowlist) — sibling feature, inverse axis.
- ADR-033 (v6 mirror of ADR-031) — shape template for v6 follow-up.
- ADR-055 (per-host egress policy templating) — the host renderer
  we extend.
- ADR-081 (operator egress bundle) — the SIGHUP-driven reload
  pattern we clone.
- ADR-082 (per-account additive egress allowlist cap) — schema +
  accessor pattern we mirror.
- ADR-092 (Gate-B cross-box mTLS hardening) — defines the
  fsn-1/fsn-2 split that v1 explicitly does NOT extend to static IP.
- ADR-100 (tenant surfaces) — child-table + quota pattern, the
  alternative we rejected for v1.
- Spec §11 (egress rules) — extend with a paragraph on the
  customer-supplied static IP path.
- `docs/scale_out_and_workload_classes.md` — explicit "any node
  can host any instance" assumption we are temporarily violating
  for static-IP customers in v1.
- Issue #757 closure (filter runtime) — JSONPath lessons transfer
  to IP validator (validate shape BEFORE walking CIDR tree).
- Issue #976 (SAFE-RELEASES) cluster — `target_deployment_id`
  pattern for additive wire fields transfers here.
- Memories: `migration-gates-collision-and-replay`,
  `cross-pr-slot-gate-races-with-active-pr`,
  `cross-pr-slot-gate-reservation-fence-pattern`,
  `cross-pr-rebase-fence-deletion-hazard`,
  `trigger-replay-safety-drop-before-create`.
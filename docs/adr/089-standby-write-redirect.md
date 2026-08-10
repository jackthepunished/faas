# ADR-089 · Standby write-redirect (Tier A9 / issue #297 slice 7)

- **Status:** accepted v1.0
- **Superseded (in part, PR-E):** prose that referred to the monolithic
  `cmd/gatewayd/` daemon is read with the Tier A7 / ADR-070 split:
  `gatewayd-public` is the TLS edge, `gatewayd-internal` is the routing
  + wake + proxy layer. The gate sits in `gatewayd-internal`'s
  `apidProxy`; the lex-min leader identity is computed by
  `pkg/gateway/leader.ElectLeader` (Tier A8 / ADR-083).
- **Date:** 2026-08-10
- **Issue:** #297 slice 7 (closes ADR-083 §Open follow-up #3: "Standby
  write-redirect")
- **Supersedes:** none
- **Related:** ADR-083 (Tier A8 active-passive topology, prerequisite),
  ADR-070 (Tier A7 edge split, prerequisite), ADR-052 (PKI / cert
  layout, prerequisite), ADR-066 (Tier A5 live-instance migration,
  related, independent), ADR-064 (Tier A4 parked-app rebalance,
  related, independent)

## Context

Tier A8 (active-passive HA, ADR-083 / PR #738) ships the
**read-side** topology: gate traffic to one box via the lex-min
leader identity, drain gracefully on operator shutdown, serve
from the survivor. What it does NOT close: **mutating writes**
that arrive at a standby.

Today, every box runs `apid` (the public mutating API) — the
active-passive layer is purely about the TLS edge
(`gatewayd-public`). A standby that receives a `POST /v1/apps`
or `POST /v1/deployments` from a stale DNS cache (TTL: 30 s
per `HADNSRecordStaleSeconds`) currently has TWO options:

1. **Process it locally.** Two replicas race on the same
   Postgres row; the second commit fails with a unique-key
   violation. Customer sees a 5xx; on retry the row exists.
   This is the silent-corruption-adjacent failure mode the
   cap-table cares about.

2. **Reject it.** 503 with a generic body. Customer SDK doesn't
   know to retry against the leader; the dashboard shows a hard
   error.

Neither is production-acceptable. ADR-083 §"Open follow-ups"
#3 explicitly lists "Standby write-redirect" as the deferred
slice worth a separate ADR. This is that ADR.

This plan ships the **deterministic relay/redirect** so a
standby's `apid` surface is identical to the leader's from the
customer's perspective, with a closed metrics vocabulary that
makes the failure mode visible to operators.

## Decision

### Topology: gate in `cmd/gatewayd-internal/apidProxy`

The gate is a single `http.Handler` constructed in
`cmd/gatewayd-internal/run.go` and wrapped around the
`apidProxy` chain after the logs-handler carve-out and before
the `isApidPath` dispatch. The gate is **opt-in** at the
deploy level: it is constructed only when
`FAAS_LEADER_REDIRECT_TLS_CERT` is set, so existing single-node
devs see no behavior change.

```
client → gatewayd-public (TLS edge) → /run/faas/gatewayd-internal.sock (H2C)
       → gatewayd-internal apidProxy → writeGate → apidProxy.next (proxyToApid)
                                       ↓
                                       leader.LeaderResolver.Current(ctx)
                                       ↓ (on standby)
                                       relayed (bearer/anonymous) ─→ mTLS hop to leader
                                       redirect_307 (cookie)     ─→ 307 to leader URL
                                       leader_unreachable         ─→ 503 + Retry-After: 60
```

The gate sits in `apidProxy` (NOT `gatewayd-public`) because
the resolver talks to Postgres, and PG access is a
gatewayd-internal surface, not a per-request hot path.
`gatewayd-public` is purely a TLS terminator; it never consults
PG.

### Eight-case decision tree

```go
type WriteOutcome int
const (
    OutcomeSameBox           WriteOutcome = iota // leader handles locally
    OutcomeRelayed                              // bearer/anonymous → mTLS hop
    OutcomeRedirect307                           // cookie → 307 to leader URL
    OutcomeLeaderUnreachable                    // no leader / DB error → 503
    OutcomeLoopPrevented                        // X-Faas-Forwarded-Leader → 508
    OutcomeMTLSFailure                           // handshake / cert chain → 503
    OutcomeError                                // catch-all → 503
)

type AuthKind int
const (
    AuthBearer    AuthKind = iota
    AuthCookie
    AuthAnonymous
)
```

The `WriteOutcome` and `AuthKind` enumerations are **closed**:

- 7 outcomes × 3 auth kinds = 21 cells.
- Pre-instantiated at boot so the dashboard surfaces zero from
  boot, not "no data".
- The `bypass` path (read methods, carve-out paths, non-apid
  paths) does NOT increment the counter — it is intentional
  silence, matching the `cold-boot-must-always-work` invariant
  (ADR-005).

For the `redirect_307` cell, the response carries:
- `Location: https://<leader.public_ip>/<path>?<query>` (built
  from the `compute_nodes.public_ip` column).
- `Retry-After: 5` (`StandbyWriteRetryAfterSeconds`).
- RFC 7807 problem-detail JSON body so SDKs can decode
  uniformly.

For the `leader_unreachable` cell:
- `503 Service Unavailable`.
- `Retry-After: 60` (`StandbyWriteNoLeaderRetryAfterSeconds`).
- RFC 7807 body with `code: leader_unreachable`.

### Lex-min leader identity (reuses Tier A8)

The gate reads the leader identity via:

```go
type LeaderResolver interface {
    Current(ctx context.Context) (name string, isMe bool, err error)
}
```

The production implementation is a `CachedLeaderResolver` that
calls `pkg/gateway/leader.ElectLeader` (Tier A8) every
`StandbyWriteLeaderURLCacheTTLSeconds = 5` and refreshes on
every `compute_node_changed` pg_notify event. The cache is
singleflight'd to avoid stampede on write bursts. The
resolver returns:

- `(name, isMe=true, nil)` — local node is the leader.
- `(name, isMe=false, nil)` — different leader; relay/redirect.
- `("", false, nil)` — empty active set (no leader elected).
- `("", false, err)` — DB / pg-notify failure.

Both empty-active and DB-error map to `OutcomeLeaderUnreachable`
per the user-locked decision (no observation about which case
the customer can act on; both are transient).

### Cross-box mTLS hop

The relay hop uses the operator-deployed client cert at
`/etc/faas/tls/gatewayd/egress-client.{crt,key}` and the CA
at `/etc/faas/tls/gatewayd/ca.crt`. **The cert CN/Directory
stays `gatewayd` per the ADR-052 PR-C+D keep set** — the
cert layout is the operator-level mTLS hop material, not
the gatewayd-daemon-specific material. Do not invent a new
cert CN or Directory in this slice.

The hop:

```go
type LeaderHTTPClient interface {
    Relay(ctx context.Context, leaderURL string, req *http.Request) (*http.Response, error)
}

type MTLSLeaderClient struct {
    httpClient *http.Client
    timeout    time.Duration  // StandbyWriteRedirectTimeoutMS = 5000
}

func NewMTLSLeaderClient(certFile, keyFile, caFile string, timeout time.Duration) (*MTLSLeaderClient, error)
```

URL construction is strict: `https://<public_ip>/<path>?<query>`.
Empty host, non-HTTPS schemes, userinfo, and malformed URLs
all error at construction time. The hop strips hop-by-hop
headers, preserves `x-faas-request-id`, and FORBIDS automatic
redirect following (`CheckRedirect` returns
`http.ErrUseLastResponse`).

### Cookie policy: 307 redirect, not relay

Cookie-authenticated requests are **redirected** to the leader's
public URL with `Retry-After: 5`. Relaying a cookie over mTLS
would leak the session cookie to the leader's request log;
redirecting keeps the cookie on the user's browser.

The check is: `r.Header.Get("Cookie") != ""`. The dashboard
shows this split with `outcome="redirect_307", auth_kind="cookie"`.

### Loop prevention: `X-Faas-Forwarded-Leader`

The relay stamp is the outbound header
`X-Faas-Forwarded-Leader: <localNodeName>`. If a relay inbound
to the leader carries this header (i.e. the leader is being
asked to relay again), the receiving leader's gate increments
`loop_prevented` and rejects with `508 Loop Detected` (no
`Retry-After` — looping is a configuration error, not a
transient one).

The stamp is set BY the outbound relay, OVERWRITING any
inbound value the customer may have spoofed. A relay that
receives a self-tagged header has been loop-prevented already;
the receiving leader's symmetric check is the load-bearing
defense.

### Cache TTL: 5 seconds, refreshed on pg_notify

The resolver cache holds for `StandbyWriteLeaderURLCacheTTLSeconds = 5`
(5 s, not 30 s, because the leader's election is also a 5-s
cache upstream per ADR-083). Every `compute_node_changed`
pg_notify event drains the cache via a non-blocking send on
the resolver's `refresh` channel.

The cache uses `singleflight.Group` so N goroutines on a
write-burst cache miss collapse into one PG query.

### Fail-closed: no-leader and DB error → 503

Both empty active set and DB / pg-notify error map to
`OutcomeLeaderUnreachable`. The response is identical
(`503 + Retry-After: 60`). The decision is operator-locked:
from the customer's perspective, both cases are "the system
is in the middle of a failover" — the dashboard surfaces the
split via the `outcome` label, but the customer response is
the same.

### Metrics closed vocabulary

```
gatewayd_internal_write_redirect_total{outcome, auth_kind}
gatewayd_internal_write_redirect_latency_seconds
```

The 7 outcomes × 3 auth kinds are **pre-instantiated** at boot
(pinned by `TestOpsMetrics_WriteRedirectPreinstantiated`). The
closed vocabulary keeps cardinality bounded; per-account
labels are explicitly out of scope.

The `mTLS_failure` outcome ALSO increments
`gateway_active_passive_failovers_total{outcome="mtls_failure"}`
so an operator chasing one metric lands on the other (ADR-089
§Decision #7 cross-link).

### `isApidPath` promoted to `pkg/apid/router.go`

The predicate was a duplicate between
`cmd/gatewayd-internal/proxy.go` and `pkg/gateway/writegate`.
PR-B promotes it to `pkg/apid/router.go::IsApidPath` as the
single source of truth, with `pkg/apid/router_test.go`
covering the anchored-root regression table. The `writegate`
package receives it as an injected `func(string) bool`
parameter on its handler constructor.

## Architectural decisions (load-bearing)

### 1. Gate sits in `apidProxy`, not `gatewayd-public`

`gatewayd-public` is purely a TLS terminator (ADR-070). The
resolver needs Postgres access, which is a gatewayd-internal
surface. Putting the gate in `gatewayd-public` would force a
PG dependency on the public listener — exactly the layering
the Tier A7 split was designed to prevent.

### 2. Cookie redirected, not relayed

A relay that stamps the cookie through mTLS leaks it to the
leader's request log. The dashboard split
(`redirect_307` vs `relayed`) makes the cache surface
visible — a customer complaining about "lots of redirects"
is a TTL/drill misconfiguration; a customer complaining about
"silent failures" is a relay/backend issue.

### 3. Loop guard overwrites inbound header

The `X-Faas-Forwarded-Leader` outbound header is set BY the
relay, not echoed from the inbound. A customer sending
`X-Faas-Forwarded-Leader: foo` does not bypass the gate —
the inbound check (`IsLoopAttempt`) fires before the
resolver, and the outbound overwrites any customer-supplied
value with the local node name.

### 4. Fail-closed on both no-leader AND DB error

The operator-locked decision: both cases map to
`OutcomeLeaderUnreachable`. The dashboard splits them via
the metric label, but the customer response is identical.
This is the "failover in progress" signal the operator can
safely advertise — the dashboard surfaces the precise cause
for the on-call.

### 5. Cache TTL = 5s, NOT 30s

The leader's identity-from-Postgres lookup is also 5s
(`StandbyWriteLeaderURLCacheTTLSeconds`, Tier A8). The
gate's resolver cache holds for the same duration so a tier
flip and a standby's first request see consistent state.
The pg_notify edge ensures the cache is invalidated
immediately on a leader change — the TTL is the safety net,
not the primary refresh path.

### 6. Bypass is intentional silence

The carve-out / read methods / non-apid paths do NOT
increment the counter. This is the same pattern as
ADR-005's "cold boot must always work" — the gate is a
write-only concern, and reads are not its surface. The
metric is `gatewayd_internal_write_redirect_total` (writes
only); no `read_total` exists.

### 7. Direct metrics cross-link on mTLS failure

The `mTLS_failure` outcome ALSO bumps
`gateway_active_passive_failovers_total`. Operators watching
the Tier A8 dashboard automatically land on the Tier A9
counter; operators watching the Tier A9 counter land on the
Tier A8 metric. ADR-083 §"Open follow-ups" #3 was: "standby
write-redirect — keep Tier A8 and Tier A9 metric dashboards
in sync so neither fails silently".

## Architecture invariants preserved (CLAUDE.md)

- **schedd remains the ONLY writer to `instances`** — the
  gate relays to the leader; the leader's `apid` is the
  only writer to `instances`. No new write paths.
- **vmmd remains the only root component** — the gate is
  userland; the mTLS hop is a userland TCP dial.
- **apid does NOT touch vmmd directly** — unchanged from
  ADR-070.
- **Two-drive rootfs (§4.6), identical inner network world
  (ADR-009), builds in ephemeral builder microVMs (ADR-003),
  cold boot must always work (ADR-005), billing = plan RAM +
  8 MB per running second (§4.7)** — all unchanged.
- **Every quota/limit in `pkg/api/limits.go`** — the four
  new limits (`StandbyWriteRedirectTimeoutMS`,
  `StandbyWriteRetryAfterSeconds`,
  `StandbyWriteLeaderURLCacheTTLSeconds`,
  `StandbyWriteNoLeaderRetryAfterSeconds`) live there, never
  inline.
- **Hard plan limits unchanged** (Free 1/1/128/5 · Hobby
  5/2/256/50 · Pro 25/5/512/250 · Scale 100/20/1024/1500).
- **Cert CN/Directory kept as `gatewayd`** per ADR-052
  PR-C+D keep set. PR-B reuses the operator-deployed
  material; no new cert files, no new Directory.

## Files to change (PR-cluster)

### Shared (PR-A + PR-B + PR-C)

- `docs/adr/089-standby-write-redirect.md` — NEW (this file).
- `docs/adr/README.md` — add slot 089 row.
- `docs/runbooks/standby-write-redirect.md` — NEW standalone
  runbook (PR-C).
- `migrations/00166_compute_nodes_public_ip.sql` — NEW
  (PR-B): additive `public_ip INET` + `public_ip_set_at
  TIMESTAMPTZ`.

### PR-A (refactor) — landed in PR #761

- `pkg/gateway/writegate` — create the package skeleton
  with `WriteOutcome`/`AuthKind` closed vocabularies,
  `LeaderResolver` interface, `IsWriteRequest`,
  `IsCarveOutPath`, `AuthKindOf`, `IsLoopAttempt`.
- `pkg/api/limits.go` — add the four `StandbyWrite*` quotas.
- `pkg/wire/metrics.go` — add `WriteRedirectTotal` and
  `WriteRedirectLatency` accessor + 7×3 pre-instantiation.
- `pkg/gateway/leader` — `ComputeNode` struct gains
  `PublicIP` field (read by the resolver; settable via the
  `gregale compute-nodes set-public-ip` CLI).

### PR-B (functional) — PR #797

- `pkg/apid/router.go` — NEW: `IsApidPath` shared predicate.
- `pkg/gateway/writegate/leader_resolver_pg.go` — NEW:
  `CachedLeaderResolver` with singleflight + RLock + 5s TTL
  + drain-on-refresh.
- `cmd/gatewayd-internal/leader_url_publisher.go` — NEW:
  background refresher on `compute_node_changed` pg_notify.
- `pkg/gateway/writegate/leader_client.go` — NEW:
  `MTLSLeaderClient` with strict URL construction, loop
  sentinel, hop-by-hop stripping.
- `cmd/gatewayd-internal/write_gate.go` + `write_gate_*.go`
  — NEW: writeGate handler with 8-case decision tree.
- `cmd/gatewayd-internal/proxy.go` — MODIFY: insert gate
  after the logs-handler carve-out.
- `cmd/gatewayd-internal/run.go` — MODIFY: construct the
  gate when `FAAS_LEADER_REDIRECT_TLS_CERT` is set.

### PR-C (test+deploy+ADR)

- `Makefile` — MODIFY: add `ha-write-redirect-drill` target.
- `deploy/lima/run-ha-write-redirect.sh` — NEW: read-only
  drill script (no `UPDATE compute_nodes` toggling).
- `docs/runbooks/standby-write-redirect.md` — NEW: 7-section
  validation matrix.
- `tests/property/write_redirect_test.go` — NEW: 5-invariant
  property test.
- `cmd/e2e/standby_write_redirect_e2e_test.go` — NEW:
  filesystem-walk e2e that asserts the runbook + ADR + drill
  + property test exist and compile.

## Failure modes (called out explicitly)

- **No active peers.** `ListActiveComputeNodes` returns empty;
  resolver returns `("", false, nil)`; gate emits
  `leader_unreachable`. Operator-facing
  `gateway_active_passive_failovers_total{outcome="no_peers"}`
  flips if the cluster stays in this state for more than
  60 s.
- **DB / pg-notify error.** Resolver returns
  `("", false, err)`; same `leader_unreachable` outcome.
  Per the user-locked decision, both cases share the
  customer-facing response.
- **mTLS handshake failure.** `MTLSLeaderClient.Relay` returns
  a TLS error; gate emits `mTLS_failure` + cross-link bump
  on `gateway_active_passive_failovers_total`. Operator
  should check `pki` cert expiry (the existing
  `pki.rotate` CLI handles renewal; the gate's runbook
  section documents the drill).
- **Loop attempt (inbound `X-Faas-Forwarded-Leader`).** Gate
  emits `loop_prevented` and 508. This is a configuration
  error — the operator's drill + runbook pin the relay
  stamp to the outbound; a leaked inbound stamp is a code
  bug.
- **Resolver cache stampede.** Mitigated by singleflight
  (PR-B `leader_resolver_pg.go`); N goroutines on cache miss
  collapse into one PG query.
- **Drill scrape race.** Known flake family (see memory note
  `vmmdgrpc-logs-happypath-coverage-flake`). The drill's
  poll loop is bounded at 30 s and tolerates one scrape
  miss.

## Hard limits

Per the CLAUDE.md "hard limits" policy, every limit in this
ADR lives in `pkg/api/limits.go` and never inline:

- `StandbyWriteRedirectTimeoutMS = 5000` — outbound mTLS hop
  timeout. Cheap relative to the customer's 30-s wake budget.
- `StandbyWriteRetryAfterSeconds = 5` — `Retry-After` on
  `redirect_307` and `mTLS_failure` / `error`. Matches the
  cookie TTL: redirect should land the customer back in
  ~5 s.
- `StandbyWriteLeaderURLCacheTTLSeconds = 5` — resolver
  cache TTL. Matches the Tier A8 leader-identity cache.
- `StandbyWriteNoLeaderRetryAfterSeconds = 60` — `Retry-After`
  on `leader_unreachable`. Long enough to ride out a normal
  failover (30 s DNS TTL); short enough that the customer
  retries while the cluster is still mostly intact.

All four are env-overridable via `FAAS_STANDBY_WRITE_*`. A bad
env panics via the limits helper so a typo doesn't silently
fall back to the default.

## Tests

- `pkg/gateway/writegate/writegate_test.go` — table-driven:
  every (outcome × auth_kind) cell, bypass path, cookie
  redirect, bearer relay, anonymous relay, no-leader, loop
  guard, mTLS failure.
- `pkg/gateway/writegate/leader_resolver_pg_test.go` —
  cache hit, miss, refresh signal, TTL expiry, empty active
  set, store error, singleflight coalescing, race-safety
  under `-race`.
- `pkg/gateway/writegate/leader_client_test.go` — success,
  timeout, TLS fail, malformed URL, HTTPS-only, loop sentinel
  propagation, no-redirect-following.
- `pkg/apid/router_test.go` — anchored-root regression
  table for `IsApidPath`.
- `tests/property/write_redirect_test.go` — 5-invariant
  fuzz: relay fires iff write AND not-me AND not-carveout
  AND not-loop AND is-apid; exactly one outcome increment
  per gated request; outcome in closed vocabulary; auth in
  closed vocabulary; same-box and carve-out paths never
  invoke the leader client.
- `cmd/e2e/standby_write_redirect_e2e_test.go` — filesystem
  walk asserting the runbook + ADR + drill + property test
  exist and compile.

The full e2e drill (`make ha-write-redirect-drill`) runs on
the two-node Lima fleet (`deploy/lima/faas-metal-2node-ha.yaml`)
on Apple Silicon M3+ via nested virt. The drill is the
**read-only** acceptance gate (per the user-locked decision):
no `UPDATE compute_nodes` toggling, assumes active-passive
already configured.

## Open follow-ups (deliberately deferred)

- **TLS-over-UNIX enforcement.** ADR-052 defers to a v1.1
  hardening slice.
- **Per-node signing keys.** Tier 2 Phase 2 / `node_signature`
  on `CapacityReport`. Out of v1.
- **ANSM SPIFFE / workload identity.** Out of v1.
- **Per-account cardinality on the write-redirect counter.**
  The closed `auth_kind` set keeps cardinality bounded;
  per-account labels are explicitly out of scope (would
  explode the metric).
- **Bin/socket/env-var rename of `cmd/gatewayd-public/`,
  `gatewayd-internal/`** (post-PR-E; the legacy narration
  sweep landed in PR #785).
- **Renaming the `gatewayd` cert CN/Directory.** Keep-set
  preserved per ADR-052 PR-C+D; a v1.1 follow-up can
  re-skin the cert material to `egress` if the operator
  surface ever demands it.

## Consequences

### Positive

- **Stale-DNS writes are deterministic.** A standby that
  receives a `POST /v1/apps` from a 30-s-cached client now
  relays (bearer/anonymous) or redirects (cookie) — never
  races the leader on the same row.
- **Operator visibility.** The 7×3 metric vocabulary plus
  the `mTLS_failure` cross-link means an on-call chasing
  "the cluster is unhealthy" lands on the specific cell
  within seconds.
- **Same mental model as Tier A8.** The operator's runbook
  is the same shape (drain event → peer picks up). Tier A9
  just adds the write-side deterministic handoff.
- **No new writes to `instances`.** The gate is pure
  routing; the leader's `apid` is the only writer. The
  schedd state machine is unchanged.

### Negative

- **Two new env vars (`FAAS_STANDBY_WRITE_*`).** Operators
  must set the four quotas in `~/.config/faas/env`. Defaults
  are sane, but the surface is new.
- **mTLS hop is a new failure domain.** A cert expiry on
  the relay client blocks the standby's relay path. The
  `mTLS_failure` outcome + cross-link surfaces this; the
  runbook's `pki.rotate` section covers the drill.
- **Loop guard restates the resolver's pg_notify contract.**
  If pg_notify is dropped, the resolver can serve stale
  identity and a relay can land on a node that flipped
  leader mid-flight. The Tier A8 same problem is mitigated
  by the leader's election-on-boot; the gate's loop guard
  is the in-flight backstop.

## Verification

### Unit

```sh
make test  # new tests: pkg/gateway/writegate, pkg/apid, pkg/wire/metrics
```

### Metal

```sh
make metal-lima-2node-ha   # existing two-node regression — must still pass
make ha-write-redirect-drill  # new: full drill
make leakcheck              # §6.2 invariant safety net — must pass
```

### Lint + spec + migrations

```sh
make lint
make spec-check         # no OpenAPI changes in Tier A9
make migrations-check   # 00166_compute_nodes_public_ip.sql additive
```

### Manual smoke (two-node Lima fleet)

1. Deploy a hello app on the leader (determined by `psql: select
   lex-min(name) from compute_nodes where active = true`).
2. `curl -H "Authorization: Bearer <drill-token>" -X POST -d
   '{...}' https://<standby>.example.com/v1/apps` → 201
   Created (relayed, response from leader).
3. `curl -d '{...}' --cookie 'faas_sid=...' https://<standby>.example.com/v1/apps`
   → 307 with `Location: https://<leader>.example.com/v1/apps`
   and `Retry-After: 5`.
4. `psql: update compute_nodes set active=false where name='<leader>'`.
5. Within 30 s:
   - `curl https://<new-leader>.example.com/v1/apps` → 200 OK.
   - `curl -s http://<new-leader>:9100/metrics | grep
     gateway_active_passive_failovers_total` →
     `outcome="dns_flipped"` ≥ 1.
   - `curl -s http://<old-leader>:9100/metrics | grep
     gatewayd_internal_write_redirect_total` →
     `outcome="loop_prevented"` ≥ 0 (loop guard silent on
     zero), OR `outcome="relayed"` ≥ 1 (bearer bouncer
     succeeded first).
6. `make leakcheck` → zero leaked netns/TAPs/cgroups.

## Acceptance

Closes the §14 M9 row "Standby write-redirect" (was deferred
in ADR-083 §"Open follow-ups" #3):

- [x] ADR-089 written (`docs/adr/089-standby-write-redirect.md`)
      (this file).
- [x] `pkg/gateway/writegate` package with `WriteOutcome`,
      `AuthKind`, `LeaderResolver`, `IsWriteRequest`,
      `IsCarveOutPath`, `IsLoopAttempt`, `AuthKindOf` (PR-A,
      PR #761).
- [x] Four `StandbyWrite*` quotas in `pkg/api/limits.go`
      (PR-A, PR #761).
- [x] `WriteRedirectTotal` + `WriteRedirectLatency` metric
      accessors with 7×3 pre-instantiation (PR-A, PR #761).
- [x] `pkg/apid/router.go::IsApidPath` shared predicate
      (PR-B).
- [x] `migrations/00166_compute_nodes_public_ip.sql`
      additive (PR-B).
- [x] `CachedLeaderResolver` with singleflight + RLock + 5s
      TTL + drain-on-refresh (PR-B, PR #797).
- [x] `LeaderURLPublisher` background refresher on
      `compute_node_changed` (PR-B, PR #797).
- [x] `MTLSLeaderClient` over operator-deployed cert
      material (PR-B, PR #797).
- [x] `writeGate` handler with 8-case decision tree (PR-B,
      PR #797).
- [x] `apidProxy.ServeHTTP` wired through the gate (PR-B,
      PR #797).
- [x] `make ha-write-redirect-drill` Makefile target (PR-C).
- [x] `deploy/lima/run-ha-write-redirect.sh` read-only
      drill script (PR-C).
- [x] Standalone runbook
      `docs/runbooks/standby-write-redirect.md` (PR-C).
- [x] Property test `tests/property/write_redirect_test.go`
      with 5 invariants (PR-C).
- [x] Runbook e2e `cmd/e2e/standby_write_redirect_e2e_test.go`
      (PR-C).
- [x] Manual smoke on Lima: relay + 307 + leader flip + zero
      5xx within 30 s.

### Cross-references

- ADR-083 (Tier A8 active-passive topology, prerequisite)
- ADR-070 (Tier A7 edge split, prerequisite)
- ADR-066 (Tier A5 live-instance migration, related)
- ADR-064 (Tier A4 parked-app rebalance, related)
- ADR-052 (PKI / cert layout, prerequisite — keep set)
- §14 M9 row: `docs/faas_implementation_spec.md`
- `docs/runbooks/active-passive-ha.md` (Tier A8 runbook; Tier A9
  is the mutating sibling)
- `docs/runbooks/multi-host-rollout.md` (mirror of the
  §14 M5 / M8 / M9 rows)

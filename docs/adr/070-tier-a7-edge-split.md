# ADR-070 · Tier A7 edge split — gatewayd-public / gatewayd-internal

- **Status:** proposed
- **Date:** 2026-08-03
- **Decision:** Split the monolithic `cmd/gatewayd` daemon into two
  single-purpose daemons connected by a unix-socket hop. The split
  is **in-process on a single box**: one box runs one `gatewayd-public`
  (TLS-only edge, the only public listener) fronting N
  `gatewayd-internal` replicas (routing + wake + proxy). Cross-box
  HA is achieved by having N boxes each with one `gatewayd-public`
  in front of their local `gatewayd-internal` set — NOT by putting
  multiple public listeners on one box.
- **Why:** The Tier A multi-box primitives (per-node schedd, snapshot
  de-localization, cross-node rebalance, cross-node live migration,
  migrating-instance watchdog — ADRs 062–067) are all in code. What's
  missing is the outer edge: how a customer HTTPS request lands on
  the right `gatewayd` when N gatewayd replicas exist on N boxes.
  Today `cmd/gatewayd` is monolithic — it owns TLS termination,
  certmagic, hostname→app routing, the wake gate, the per-node
  forwarder, and the cert storage. Putting N copies of that daemon
  behind an external LB gives us the §11 "single public listener"
  invariant for free but breaks per-process state (rate limiter
  buckets split, warm hints split, cert storage split, mTLS-to-vmmd
  not handled). Splitting it in-process — one `gatewayd-public`
  (TLS-only) fronting N `gatewayd-internal` replicas (routing +
  wake + proxy) over a unix-socket hop — keeps the existing
  forwarder shape, gives us a clean cert-renew race model, and lets
  us keep "the only public listener" as a per-box invariant
  (`gatewayd-public` is still ONE process surface per box).
- **Consequences:**
  - Two new daemons: `cmd/gatewayd-public` and `cmd/gatewayd-internal`.
    Legacy `cmd/gatewayd` stays in-tree for the migration window
    (the legacy and split daemons run side-by-side; the LB points
    at `gatewayd-public` once the operator flips the env).
  - Three new library files: `pkg/gateway/internal_proxy.go`
    (public→internal reverse-proxy over unix socket),
    `pkg/gateway/readiness.go` (probe + PG ping + staleness signals),
    `pkg/gateway/routes_hydration.go` (route-cache hydration
    tracker), and `pkg/gateway/certsync/certsync.go` (leader
    election + cert replication).
  - Three new migrations: 00116 `warm_hint` (sticky-warm hint
    table + `warm_hint_published` pg_notify), 00117
    `pg_ratelimit_counters` (centralized rate-limit counters,
    opt-in via `[ratelimit] mode = "central"`), 00118 reserve
    slot (follow-on ADR-069/070/071 fences).
  - Four new constants in `pkg/api/limits.go`:
    `GatewayDrainGraceSeconds=25`, `ReplicaHeartbeatIntervalSeconds=5`,
    `WarmHintCacheSize=1000`, `CertSyncIntervalSeconds=30`.
  - CLAUDE.md §"Component ownership" invariant rewrites:
    "gatewayd is the only public listener on the box" →
    "the platform exposes exactly ONE public ingress per box; that
    ingress is `gatewayd-public`. Internal-only `gatewayd-internal`
    listens on a unix socket inside the box. Cross-box remote
    ingress is `gatewayd-public` on the other box's public IP."
  - The pre-split `/readyz` always-200 default is **inverted** to
    always-503 when no probe is wired. The legacy daemon is
    migrated to a real probe (`deps.pgStore != nil`). The new
    daemons wire the full probe from `pkg/gateway/readiness.go`.
  - Two new systemd units (`faas-gatewayd-public.service`,
    `faas-gatewayd-internal.service`) + two new ansible roles
    (`gatewayd_public_service`, `gatewayd_internal_service`).
  - Slot-neutral for the LEGACY migration set; **this PR cluster
    ships 116–118 as the new edge-tier slots** (the 116 reservation
    from the open issue #517 follow-up is preserved; we add 116
    fresh here). Cross-PR slot fence pattern documented below.

## Architectural decisions

1. **In-process split, not multi-process on one box.** We chose to
   keep "one public listener per box" as the load-bearing invariant
   (CLAUDE.md §11). The split happens INSIDE the public-listener
   process: `gatewayd-public` owns TLS + certmagic; everything
   inside (routing, wake, forwarder, rate limit) lives in
   `gatewayd-internal`. This matches the §11 single-public-listener
   rule without inventing a new perimeter.
2. **Unix-socket hop, same box only in v1.0.** `gatewayd-public`
   dials `/run/faas/gatewayd-internal.sock` (ADR-015/018 pattern,
   mode 0660 + group faas) to forward every inbound request. The
   unix-socket hop is HTTP/1.1 over the unix socket — same shape
   as `pkg/sched/loop.go::httpGatewaySynth` already uses for the
   cron synth server. Cross-box mTLS hop is Gate-B work.
3. **Sticky-warm routing.** `gatewayd-public` reads the warm hint
   for the app and picks the internal replica whose
   `compute_node.id` matches the hint. The hint is published via a
   new `warm_hint` table (migration 00116) + `warm_hint_published`
   pg_notify channel; both daemons subscribe independently. On
   cache miss / no warm, the public daemon hashes `host||ip` to a
   replica (consistent hashing on the internal replica registry)
   and retries on the next replica if the chosen one fails
   `/readyz`.
4. **Internal replica registry.** Each `gatewayd-internal` posts a
   registration at startup over a unix socket
   (`/run/faas/gatewayd-public-replica.sock`) carrying
   `{compute_node_id, advertise_addr, started_at}`. The public
   daemon keeps a `nodeID → replica` map with a 5 s heartbeat
   (driven by `api.ReplicaHeartbeatIntervalSeconds`). Stale
   (>10 s without heartbeat) → marked unready → excluded from hash
   and warm-hint resolution. No new pg_notify channel; pure local
   pubsub.
5. **Centralized rate limit (opt-in).** Today's per-process token
   bucket (`pkg/gateway/ratelimit.go`) is correct for one-box. After
   the split, sticky-by-warm-node routing does NOT pin a single
   replica, so per-process buckets see a fraction of customer
   traffic and the limit leaks. The fix is opt-in via
   `[ratelimit] mode = "central"` in TOML and uses a Postgres-
   backed counter (migration 00117 — `pg_ratelimit_counters`).
   Default stays "process" (today's behaviour); operators flip when
   they go multi-box. Same staged-rollout risk mitigation as
   ADR-040.
6. **Cert replication (per-replica FileStorage + leader-by-lex-min).**
   Each `gatewayd-public` runs `NewCertMagicConfig` independently
   against its own `StorageDir` (per-replica at
   `/var/lib/faas/certs/<replica-id>/`). The leader-elected replica
   (lex-min `compute_node.id` among active public daemons) owns
   renewal; followers replicate via a tiny unix-socket wire format
   (ADR-069 — `pkg/gateway/certsync`). The shared
   `CAConfigDir` (`/var/lib/faas/ca/`) carries the ACME account
   key — only the leader writes, followers read.
7. **Readiness inversion.** The pre-split `/readyz` always-200
   default was a latent bug (cmd/gatewayd/main.go:878 wired `nil`).
   After the split, a partial-boot daemon must NOT accept traffic
   even if the LB scrape is intermittent. The new contract is:
   - `nil` probe → 503 (daemon forgot to wire a probe — wiring bug)
   - registered probe that returns false → 503 (with the most
     recent reason from any failing component)
   - registered probe that returns true → 200
   The pre-split daemon is migrated to a real probe
   (`deps.pgStore != nil`); the new daemons wire the full probe
   from `pkg/gateway/readiness.go` (PG ping + cache hydration +
   schedd router readiness + internal proxy dial).
8. **Migration slots.** This PR cluster ships 116–118:
   - 00116 `warm_hint` (table + pg_notify channel)
   - 00117 `pg_ratelimit_counters` (central rate-limit table)
   - 00118 reserve slot (follow-on ADR-069/070/071 fence)
   The pre-existing 116 reservation from the open issue #517
   follow-up is **preserved as 116 here** (same slot — the
   reservation is dropped on rebase per ADR-041). Cross-PR slot
   fence pattern (PR #391): rename + add no-op `select 1;` fence so
   the embedded set stays contiguous 1..N.
9. **Drain order.** Internal-first, public-second. `gatewayd-internal`
   drains on SIGTERM (sets `/readyz=503`, stops accepting from the
   unix socket, waits in-flight, posts `deregister` to
   `gatewayd-public`, exits). `gatewayd-public` then drains (sets
   `/readyz=503`, stops accepting on :443/:80, waits in-flight,
   exits). The order matters: if the public daemon drains first,
   in-flight requests die on the unix socket.
10. **Network surface hardening.**
    `faas-gatewayd-internal.service` carries
    `RestrictAddressFamilies=AF_UNIX` — even a buggy code path
    can't reach an external IP from the internal daemon. The
    legacy `faas-gatewayd.service` was `AF_INET AF_INET6 AF_UNIX`
    because the legacy daemon dials external IPs through the
    forwarder; the internal daemon's only outbound is gRPC to
    per-node schedd/vmmd via `pkg/wire.DialContext` (loopback mTLS
    or unix socket).
11. **Listener boundaries.** `gatewayd-public` owns:
    - `:80` ACME HTTP-01 + `.well-known/acme-challenge/*`
    - `:443` TLS termination with certmagic `GetCertificate`
    - `/healthz`, `/readyz`, `/metrics` on loopback `:9090`
    - `pkg/httpsec` outer wrapper (HSTS / CSP nonce /
      X-Frame-Options / Referrer-Policy / X-Content-Type-Options /
      Permissions-Policy)
    `gatewayd-internal` owns:
    - `/run/faas/gatewayd-internal.sock` (HTTP/1.1)
    - `/healthz`, `/readyz`, `/metrics` on loopback `:9091`
    - the entire `pkg/gateway/handler.go` surface
      (hostname→app, wake gate, rate limit, forwarder)

## Configuration

| Env var | Default | Purpose |
|---|---|---|
| `FAAS_PUBLIC_LISTEN_ADDR` | `:443` | `gatewayd-public`'s TLS listener. |
| `FAAS_INTERNAL_SOCKET` | `/run/faas/gatewayd-internal.sock` | Unix socket the public daemon dials. |
| `FAAS_INTERNAL_CONTROL_ADDR` | `127.0.0.1:9091` | `gatewayd-internal`'s control listener. |
| `FAAS_CERT_STORAGE_DIR` | `/var/lib/faas/certs` | Per-replica certmagic storage. |
| `FAAS_APPS_DOMAIN` | `apps.gregale.dev` | DNS-01 mint zone. |
| `FAAS_HETZNER_DNS_TOKEN_PATH` | `/etc/faas/hetzner-dns.token` | DNS-01 solver token. |
| `FAAS_ACME_CONTACT_EMAIL` | `ops@<apps_domain>` | CertMagic registration email. |
| `FAAS_NODE_ID` | (PG lookup) | Override for compute_nodes.id when PG is unreachable at boot. |
| `FAAS_NODE_NAME` | `default-local` | Override for compute_nodes.name. |
| `[ratelimit] mode` | `process` | `process` (legacy) or `central` (Postgres-backed counter). |

Constants live in `pkg/api/limits.go` (canonical hard-limits table;
never inline a limit per CLAUDE.md).

## State surface

- `warm_hint` table (migration 00116) — sticky-warm publication.
  Primary key `(app_id)`; CHECK on `written_at <= now()+1 minute`
  for clock-skew safety; partial index on `node_id` WHERE
  `node_id IS NOT NULL` for the future "list all apps warm on node
  X" query (operator dashboard).
- `warm_hint_published` pg_notify channel — published by schedd's
  `Broadcaster` (pkg/sched/warmhint.go) on every successful emit.
  Consumed by `gatewayd-public` (route-cache mirror at the edge)
  and `gatewayd-internal` (the existing StreamWarmHints consumer,
  migrated to the new pg_notify path).
- `pg_ratelimit_counters` table (migration 00117) — opt-in
  centralized rate-limit counters. PRIMARY KEY
  `(scope, subject_id, plan)`; CHECK on `scope ∈ {'app','account'}`;
  CHECK on `plan ∈ {free,hobby,pro,scale}`; CHECK on `tokens >= 0`;
  partial index on `subject_id` WHERE `scope = 'app'`. The
  consume path is `INSERT … ON CONFLICT … DO UPDATE SET tokens =
  tokens + delta … RETURNING tokens` wrapped in
  `pg_advisory_xact_lock(hashtext((scope,subject_id,plan)::record))`
  so two replicas contending on the same row serialise.

## Wire / proto

- **Zero new proto.** All wire changes are local to
  `pkg/gateway/` (the reverse-proxy library and the cert-sync wire
  format). The unix-socket hop is HTTP/1.1 over the existing
  `pkg/sched/loop.go::httpGatewaySynth` pattern. The cert-sync
  wire format is a 24-byte fixed header + concatenated PEMs
  (defined in `pkg/gateway/certsync/certsync.go::EncodeWire`).

## Critical files

- `pkg/gateway/readiness.go` (new) — `ReadySignal`, `ReadyzProbe`,
  `NewStalenessSignal`, `NewPGPingSignal`.
- `pkg/gateway/readiness_test.go` (new) — race-clean unit tests.
- `pkg/gateway/routes_hydration.go` (new) — `RouteCacheHydration`
  tracker + `RouteCacheLoader` seam.
- `pkg/gateway/routes_hydration_test.go` (new) — race-clean unit tests.
- `pkg/gateway/control.go` (edited) — inverted nil-ready default.
- `pkg/gateway/control_test.go` (edited) — updated nil-ready
  subtest to assert 503.
- `pkg/gateway/internal_proxy.go` (new) — public→internal reverse
  proxy over unix socket.
- `pkg/gateway/internal_proxy_test.go` (new) — hop-by-hop,
  X-Forwarded-For append, dial-failure 502, upstream 5xx pass-through.
- `pkg/gateway/certsync/certsync.go` (new) — leader election,
  peer sync wire format, file writer.
- `pkg/gateway/certsync/certsync_test.go` (new) — lex-min election,
  wire round-trip, magic + version rejection, file writer.
- `cmd/gatewayd/main.go` (edited) — wired real `ReadyFunc` at
  line 878 (was `nil`).
- `cmd/gatewayd-public/main.go` (new) — TLS-only edge daemon.
- `cmd/gatewayd-public/` — bootstraps certmagic, httpsec, the
  internal-proxy, the certsync leader, and the readiness probe.
- `cmd/gatewayd-internal/main.go` (new) — routing + wake + proxy
  daemon (skeleton; the handler file moves land in a follow-on PR).
- `pkg/api/limits.go` (edited) — 4 new constants.
- `migrations/00116_warm_hint.sql` (new) — `warm_hint` table +
  CHECK + partial index.
- `migrations/00117_pg_ratelimit.sql` (new) —
  `pg_ratelimit_counters` table.
- `migrations/00118_reserve_slot.sql` (new) — follow-on ADR fence.
- `deploy/systemd/faas-gatewayd-public.service` (new) — TLS-only
  edge unit with `CAP_NET_BIND_SERVICE` + cert storage
  ReadWritePaths.
- `deploy/systemd/faas-gatewayd-internal.service` (new) — unix-only
  internal unit with `RestrictAddressFamilies=AF_UNIX`.
- `deploy/ansible/roles/gatewayd_public_service/` (new) — ansible
  role for storage dirs + unit drop.
- `deploy/ansible/roles/gatewayd_internal_service/` (new) — ansible
  role for the unix-socket-only unit.
- `CLAUDE.md` (edited) — Component ownership invariant rewrite.

## Tests

- `pkg/gateway/readiness_test.go`:
  - `TestReadyzProbe_All_EmptyReturnsTrue` — empty probe reports
    ready (pre-split behaviour preserved for early-boot compatibility).
  - `TestReadyzProbe_Register_DefaultsNotReady` — every newly
    registered signal starts at not-ready.
  - `TestReadyzProbe_All_FoldsSignals` — fan-in: every signal must
    be ready for All() to return true.
  - `TestReadyzProbe_All_ConcatsReasons` — operator-visible
    reasons joined with `"; "`.
  - `TestReadyzProbe_ReadyFunc_StableUnderConcurrency` — race-clean
    read path.
  - `TestNewPGPingSignal_*` — flips on success, flips on error,
    stopper flips not-ready on drain.
  - `TestNewStalenessSignal_*` — fresh touch keeps ready; staleness
    flips not-ready; touch recovers.
- `pkg/gateway/routes_hydration_test.go`:
  - `TestRouteCacheHydration_*` — defaults, MarkHydrated,
    MarkUnhydrated, idempotency.
  - `TestRouteCacheLoader_Contract_OnSuccess` / `_OnFailure` —
    success hydrates + populates; failure keeps not-hydrated and
    surfaces the reason.
- `pkg/gateway/control_test.go` (updated):
  - The pre-existing `ready by default` subtest is renamed to
    `not-ready when no callback registered` and asserts 503 with
    body containing `no probe registered`.
- `pkg/gateway/internal_proxy_test.go`:
  - Hop-by-hop header stripping.
  - X-Forwarded-For append (not replace).
  - Dial failure → 502 with `internal dial failed` body.
  - Upstream 5xx → propagated unchanged.
  - Nil dialer / nil target → 502 wiring bug.
  - `TestNewUnixSocketDialer_RespectsContextCancel` — ctx-cancel
    abort within 100 ms.
  - `TestIsHopByHop_Predicate` — the lookup table.
- `pkg/gateway/certsync/certsync_test.go`:
  - `TestLeader_LexMinElection` / `_Follower` / `_EmptyLister` /
    `_RecomputeError` / `_PeersExcludesLeader` — election
    semantics.
  - `TestLeader_Renew_FollowerRejected` — `ErrNotLeader` for
    followers; closure NOT called.
  - `TestLeader_Renew_LeaderDelegates` — leader passes closure
    through.
  - `TestEncodeDecodeWire_RoundTrip` — wire round-trip.
  - `TestDecodeWire_BadMagic` / `_BadVersion` / `_ShortBuffer` —
    rejection safety.
  - `TestWriteCertAndKeyToDisk` — file writer + 0600 perm check.
  - `TestLeader_ConcurrentReads` — race-clean.

## Verification

- `make test` — full unit suite, must pass with `-race`.
- `make test-metal` — exercise the legacy + split daemons on
  Lima / EX44 (the legacy daemon stays in-tree during the
  migration window).
- `make leakcheck` — zero leaked netns/TAPs/cgroups.
- `make lint` — `golangci-lint` + repo-wide `gofmt -l` gate.
- `make spec-check` — vacuum + AST parity + git clean.
- `make migrations-check` — embedded set stays contiguous 1..118.
- Manual smoke (Lima / EX44):
  1. `make bootstrap && make run` with one `gatewayd-public` +
     one `gatewayd-internal`.
  2. `curl -s http://127.0.0.1:9090/readyz` → 200 after PG ping
     succeeds.
  3. `curl -s https://example.apps.gregale.dev/ | head -5` →
     customer app response.
  4. Kill `gatewayd-internal`; `curl -s https://...` → 502 Bad
     Gateway (`internal dial failed`).
  5. Restart `gatewayd-internal`; `curl -s https://...` → 200.

## Migration slot

**116–118.** This PR cluster ships three migrations:

- 00116 `warm_hint` (table + pg_notify)
- 00117 `pg_ratelimit_counters`
- 00118 reserve slot (follow-on ADR-069/070/071 fence)

The pre-existing 116 reservation from the open issue #517
follow-up is **preserved as 116 here** — the reservation is
dropped on rebase per ADR-041 / PR #391 carve-out. The follow-on
PRs (cert-bundle audit, hint retention, LB-coordination tokens
per ADR-069/070/071) claim slots 119+; their renumber chain
follows the existing post-#533-merge renumber-reset pattern.

## Open follow-ups (deliberately deferred)

- Cross-box `gatewayd-public` ↔ `gatewayd-internal` mTLS hop
  (Gate-B; out of scope).
- Cross-region `gatewayd-public` replication (Gate-B; out of scope).
- Capacity-aware internal-replica selection so the public daemon
  doesn't pick a saturated replica even when the warm hint says
  otherwise (Tier A8 follow-on).
- The full file moves from `cmd/gatewayd/` to `cmd/gatewayd-internal/`
  (the handler, pgbackend, scheddrouter, nodecache, forwarder, etc.)
  land in a separate PR cluster to keep review surface small.
- HAProxy / cloud-LB story for operators who want to put
  `gatewayd-public` behind an external LB anyway (deferred to ops docs).
- A "soft" version of sticky-by-app that allows hot-app replicas
  to spread to multiple internal replicas if their load diverges
  (cap on per-replica saturation).

## Rejected alternatives

- **Multi-process on one box (`gatewayd-public` × N + `gatewayd-internal`
  × N behind an external LB).** Rejected — violates CLAUDE.md
  "single public listener per box" and breaks per-process state
  (rate limiter buckets split, warm hints split, cert storage
  split). The whole point of the split is to KEEP single-process
  state on the internal tier while presenting a single TLS surface
  to the public.
- **`apps.last_warm_node_id` column.** Rejected — same writer-
  invariant problem (would force schedd to write a customer-intent
  table); also breaks schedd/apid writer roles.
- **Sticky-by-app routing.** Rejected for v1.0 — sticky-by-warm-
  node is the brief; sticky-by-app with a rebalanced app = new
  replica ≠ old bucket = broken invariant.
- **Centralized rate limit in Redis.** Rejected — new dependency,
  out of scope. The Postgres-backed counter has the right
  latency characteristics (P50 0.8 ms, P99 3.2 ms on EX44 per
  ADR-040 follow-up bench).
- **Single combined `gatewayd-public` + `gatewayd-internal`
  process that switches mode via env.** Rejected — the two
  daemons have different resource limits, different systemd
  units, different restart policies. A combined process can't
  restart one tier without restarting the other.
- **Don't split — just put the legacy daemon behind an external
  LB.** Rejected — per-process state breaks (rate limit buckets
  split, warm hints split). The brief asks for a clean
  state-isolation model; the split is the only path that
  delivers it.
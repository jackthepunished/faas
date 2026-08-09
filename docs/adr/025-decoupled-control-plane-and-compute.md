# ADR-025 · Decoupled Control Plane and Compute Nodes

- **Status:** accepted v1.1 (2026-07-31). v1.1 adds the §6.4 failure-mode catalogue (spec §6.4); v1.0 was the original three-axis decoupling.

> **⚠️ Status flips do not authorise a real cutover.** The first second control-plane node is gated by the [Tier 2 pre-requisites](#tier-2-pre-requisites) below and the runbook `docs/runbooks/multi-host-rollout.md`. As of 2026-08-09 all five Tier 1 phases + off-host PG backup are shipped and the runbook is the operator-facing reference for the multi-host cutover — see `docs/runbooks/multi-host-rollout.md` §3 + §4 for the bootstrap procedure, and §3.5 for the `listen_addr`/`target_url` distinction the operator must understand to avoid the wrong-host routing trap.
- **Date:** 2026-07-21 (proposed); 2026-07-31 (accepted v1.1)
- **Decision:** Evolve the FaaS architecture from a strict single-box loopback deployment to a decoupled, location-transparent topology. Specifically:
  - Transition the internal service-to-service gRPC boundaries (e.g. `schedd` ➔ `vmmd`, `builderd` ➔ `vmmd`) from hardcoded UNIX domain sockets to support standard TCP/IP networking secured via **Mutual TLS (mTLS)**.
  - Abstract local filesystem writes for rootfs layers and VM snapshot storage behind a unified storage interface (`StorageBackend`). Support local disk storage for single-box mode, and an OCI registry or object-storage-backed driver for distributed deployments.
  - Abstract edge routing in `gatewayd` to optionally tunnel or route network traffic via system-level mesh overlays (such as WireGuard or Cilium/VxLAN) when tenant guest TAP interfaces run on remote physical hosts.
  - Maintain absolute backwards compatibility with the existing single-box deployment mode, ensuring local developer loops and integration test suites run without modifications using localhost/loopback sockets.
- **Why:** The current prototype (Milestones M0 to M8) is structurally hardcoupled to a single host. Compute-bound services (`vmmd` and Firecracker microVMs) require hardware virtualization (`/dev/kvm`), which is unavailable or expensive on standard cloud VPS offerings (e.g. regular DigitalOcean Droplets). Decoupling the compute nodes allows developers to run control-plane services (`apid`, `gatewayd`, `schedd`, etc.) on inexpensive, standard cloud servers while routing virtualization workloads to dedicated hardware hosts or cloud instances that support nested virtualization (such as Intel GCP N1/N2 VMs).
- **Consequences:**
  - **Location Transparency:** Services can be run anywhere. The same system can be deployed on a single physical host or distributed across multiple cloud providers.
  - **Security (mTLS):** Moving gRPC communication over TCP introduces a network boundary. Services MUST enforce certificate verification via mutual TLS (mTLS) to prevent unauthorized control-plane calls.
  - **Shared Registry/Storage:** Introducing a remote storage driver eliminates the local disk dependency. Compute nodes pull required app and base layers on-demand, making compute nodes stateless and easily scalable.
  - **Config Additions:** `schedd` and `vmmd` gain standard gRPC server/client parameters (such as `listen_network`, `cert_file`, `key_file`, `ca_file`).

## Tier 2 pre-requisites

The following are still un-shipped at v1.1 time and gate the load-bearing failure modes in [spec §6.4](https://github.com/poyrazK/faas/blob/main/docs/faas_implementation_spec.md#6.4-failure-mode-catalogue-adr-025-v11-adr-028-v11-adr-029-v11). When each ships, retire the corresponding bullet from this list and from the [Tier 2 plan](https://github.com/poyrazK/faas/issues/297) cross-references.

- **Tier 1 Phase 2** (`node_signature` on `CapacityReport`) — **RETIRED 2026-08-09** by ADR-053 acceptance (v1.0) and ADR-058 Tier A1 acceptance. The cryptographic stamp + the chooser ledger-floor (`max(live, ledger.ResidentRAMForNode)`) ship together; the chooser no longer trusts stale `Σ(ram_mb+8)` sums. See ADR-053 §1 and ADR-058 §Decision.

- **Tier 1 Phase 1** (cross-host mTLS + handler-layer peer binding) — **RETIRED 2026-08-09** by ADR-052 acceptance (v1.0). Stdlib verifier does chain + RFC 6125 SAN + EKU in a single handshake pass; the handler-layer `pkg/wire.PeerCN(ctx)` helper binds the peer CN to a registered role for defense-in-depth. Every dial/listen site listed in ADR-052 §Dial/listen sites is wired via `wire.LoadClientTLSConfig*` / `wire.LoadServerTLSConfig*`. The operator workflow ships via `gregale pki init|status|rotate` (`cmd/gregale/commands_pki.go`). Single-box default-local deployments stay on `unix://` sockets; multi-box deployments flip to TCP/DNS + the three cert/key/CA fields. Acceptance verified by `cmd/e2e/mtls_e2e_test.go` on real TCP `127.0.0.1` and `pkg/wire/grpc_test.go::TestMTLSRoundTripPeerCN`. See ADR-052 §Decision and `docs/runbooks/multi-host-rollout.md` §Pre-conditions.

- **Tier 1 Phase 5** (`pkg/wire.NodeVerifier` handshake-layer CN→`compute_nodes.name` binding) — **RETIRED 2026-08-09** by ADR-056-wire-node-verifier acceptance (v1.0). The verifier augments (never replaces) stdlib chain/SAN/EKU trust and runs in `tls.Config.VerifyPeerCertificate` so rejection happens BEFORE any RPC dispatch. Single-box dev keeps `AllowAllNodeVerifier`; multi-box schedd + vmmd wire `PGNodeVerifier` (snapshot-backed, loader-failure-keeps-last-known-good). Defense-in-depth: handler-layer `wire.PeerCN(ctx)` (ADR-052) stays untouched. See `docs/adr/056-wire-node-verifier.md` (filename canonical — slot 056 is also claimed by `056-off-host-pg-backup.md`; see `docs/adr/068-issue-517-closure-evidence.md` §"Note on slot-collision hygiene").

- **Tier 1 Phase 3** (`OCIRegistryStorageBackend` end-to-end) — blocks the "Snapshot locality" row in §6.4. Mitigates the case where a compute node cold-boots without the per-app layer.
- **Tier 1 Phase 4** (per-host egress policy templating) — blocks the "Egress policy per host" row in §6.4. Mitigates the case where one host's `policy_nftables.conf` references another host's `MasqueradeCIDR`.
- **#250** (off-host Postgres backup) — **RETIRED 2026-08-09** by ADR-056 acceptance (v1.0). The compound `archive_command` (cp + rclone to Hetzner Storage Box), `faas-pg-basebackup-push.{service,timer}` pair, sealed-at-rest `/etc/faas/secrets/storage-box/` credentials, `pg_backup_last_pushed_seconds` gauge + `PgBackupStale` alert, and `pg-restore-verify.sh` T-7 throwaway verify are all in tree. See ADR-056 §Load-bearing design choices and `docs/runbooks/PostgresBackup.md`.

**PR #425** (closed-not-merged 2026-07-29) was the prior attempt at this status flip; it lacked the callout above. This v1.1 supersedes it.

---

## Technical Details

### 1. Dialing & Listening Abstraction

Currently, `pkg/scheddgrpc` dials a hardcoded UNIX path:
```go
// Pre-slice-1 dialer code (now superseded)
conn, err := grpc.Dial("unix://" + socketPath, grpc.WithInsecure())
```

Slice 1 (landed) extended dialing to parse a target address URL scheme
(`unix://`, `tcp://`, `dns://`) and shipped the real surface in
`pkg/wire/grpc.go`:

```go
// Real dial helper — see pkg/wire/grpc.go for the full implementation.
func DialContext(ctx context.Context, target string, tlsCfg *tls.Config, opts ...grpc.DialOption) (*grpc.ClientConn, error)
func Listen(ctx context.Context, target string, tlsCfg *tls.Config) (net.Listener, error)
func LoadClientTLSConfig(certPath, keyPath, caPath string) (*tls.Config, error)
func LoadServerTLSConfig(certPath, keyPath, caPath string) (*tls.Config, error)
```

TCP/DNS targets require a non-nil `tlsCfg`; the loader returns a
`*tls.Config` populated with the operator's CA pool and the standard
stdlib verifier (chain trust, RFC 6125 SAN matching, EKU enforcement —
all in a single handshake pass). See ADR-052 §Handler-layer peer binding
for the per-service `compute_node.id` CN-binding that runs *after* the
handshake, and §Reference call sites for the per-daemon TLS threading.
        opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
    }
    return grpc.Dial(target, opts...)
}
```

### 2. Config Schema Extensions

#### `vmmd.toml` Updates:
```toml
# Network bind options
listen_addr = "127.0.0.1:50051"     # or "unix:///run/faas/vmmd.sock"
tls_cert_path = ""                  # Path to server certificate (optional)
tls_key_path = ""                   # Path to server private key (optional)
tls_ca_path = ""                    # Path to client CA certificate (optional for mTLS)
```

#### `schedd.toml` Updates:
```toml
# Remote vmmd target
vmmd_target = "unix:///run/faas/vmmd.sock" # or "tcp://10.128.0.5:50051"
vmmd_tls_cert_path = ""                    # Client certificate (optional)
vmmd_tls_key_path = ""                     # Client private key (optional)
vmmd_tls_ca_path = ""                      # Server CA certificate (optional)
```

### 3. File System Decoupling

We introduce the `StorageBackend` interface in `pkg/storage`:
```go
type StorageBackend interface {
    Put(ctx context.Context, key string, r io.Reader) error
    Get(ctx context.Context, key string) (io.ReadCloser, error)
    Delete(ctx context.Context, key string) error
}
```

*   `LocalStorageBackend`: Mounts local directory and writes files directly (used for single-box mode).
*   `OCIRegistryStorageBackend`: Wraps pushing/pulling images as OCI layers.

---

## Future Scaling & Multi-Node Orchestration

This decoupling directly unlocks horizontal scaling of the compute layer:
1. **Multi-Node Scheduling:** `schedd` will be upgraded from a single-box allocator to a multi-node scheduler. It will track the capacity and resources (vCPU, memory headroom, slot allocations) of all registered compute nodes, dispatching wake and create commands to the node selected by its placement algorithm.
2. **Cross-Node Routing:** The routing registry will map active instances to their host compute node's private IP. When `gatewayd` receives a request, it resolves the destination to the correct remote compute node IP and routes traffic across the private overlay network.
3. **Stateless Compute Nodes:** Because the filesystem is decoupled via the `StorageBackend` abstraction, compute nodes do not hold persistent customer state. They act as stateless execution runtimes that pull image layers on-demand, allowing new dedicated servers to be added to the cluster instantly.

---

## Rejected Alternatives

- **Always TCP (remove UNIX socket support):**
  - Rejected because UNIX domain sockets are faster, simpler, and provide OS-level file permission boundaries on single-box setups. We must retain UNIX socket support.
- **Plain TCP (no mTLS):**
  - Rejected. Exposing `vmmd`'s control surface (which runs as root and boots VMs) over plain unauthenticated TCP creates a critical vulnerability. Strong certificate-based authorization is mandatory for distributed setups.
- **CA-only verification (chain with no hostname pinning):**
  - Rejected. The first iteration of `loadClientTLSConfig` did chain-only verification by suppressing stdlib's hostname check (`InsecureSkipVerify=true`) and re-running chain validation in a custom `VerifyPeerCertificate` hook. That posture was strictly weaker than letting stdlib's default verifier run, and CodeQL alert #58 flagged the literal `= true` regardless of any rationale. The current design (issue #95, slice 1) relies on stdlib's `verifyServerCertificate` (handshake_client.go / handshake_client_tls13.go), which performs chain trust against the operator's `RootCAs`, RFC 6125 SAN matching, and EKU enforcement in a single pass during the handshake. grpc-go's `tlsCreds.ClientHandshake` populates `ServerName` from the dial `:authority` before `tls.Client` is called, so no caller-side plumbing is required.
  - **Operational consequence:** distributed deployments must issue per-daemon SANs (`schedd.faas`, `vmmd.faas`, …) on every leaf certificate. The local-dev PKI continues to issue SANs for `127.0.0.1`, `::1`, and `localhost`, so single-box tests stay correct. A future slice that adds a production-ready dev PKI for distributed setups should make this automatic.

---

## 4. Live capacity reporting (axis 5)

### 4.1 Decision

`vmmd` pushes a `CapacityReport` (live_count, leased_count, used_mb, ram_headroom_mb, vcpu_busy) to `schedd` every 1 s on a client-streaming gRPC RPC (`Schedd.ReportCapacity`). `schedd` updates a per-node in-memory cache that the placement chooser reads before falling back to the legacy store-derived sum.

This slice is the load-bearing fix for issue #297 / ADR-025 v1.1 acceptance #1: the chooser must read live RAM headroom, live instance count, and vCPU availability from each compute node, not the stale seeded values written at `vmmd` registration and not the stale sum-of-plan-MBs derived from `instances.ram_mb + 8`.

### 4.2 Wire shape

Client-streaming (`vmmd` is the gRPC client). Proto additive per ADR-016:

```proto
rpc ReportCapacity(stream CapacityReport) returns (ReportCapacityAck);

message CapacityReport {
  string node_id             = 1;  // compute_nodes.id (uuid)
  int64  sampled_at_unix_ms  = 2;  // informational; chooser uses staleness, not absolute time
  int32  live_count          = 3;  // Manager.LiveCount()
  int32  leased_count        = 4;  // Manager.LeasedCount()
  int32  used_mb             = 5;  // Σ(memory.current / 1 MiB) across live instances
  int32  ram_headroom_mb     = 6;  // cfg.ComputeNode.MemMB - used_mb, clamped at 0
  int32  vcpu_busy           = 7;  // v1: live_count * 2 (conservative placeholder)
}

message ReportCapacityAck {}
```

Server-streaming was rejected because it forces ack frames per tick and inverts producer/consumer semantics. Reusing the existing 200 ms `instancestats` poller was rejected because freshness ties to the poller's lifecycle and the wording "vmmd reports" calls for a producer on `vmmd`.

### 4.3 Trust model

The reverse direction (vmmd→schedd) is new for this codebase. The existing schedd→vmmd `Heartbeat` RPC is pull-only (vmmd.proto:63-70, sched.heartbeat.go:16-19) with a documented rejection rationale ("schedd is the admission authority and shouldn't trust inbound traffic from a box it may have already drained"). We accept the new direction for capacity *because* capacity is bias, not authority:

- The chooser reads capacity as one input to `ChoosePlacement`, never as the only input. The per-node `AdmissionCeilingMB` check inside `ChoosePlacement` (placement.go:104-107) and the per-node ceiling inside `NodeLedger.Admit` (admission.go:165-169) are the load-bearing enforcement; capacity reports inform, never gate.
- The ledger floor rule (PR-2's `applyLiveCapacityMB`) caps trust: a vmmd report's `used_mb` is taken as `max(report.used_mb, ledger.ResidentRAMForNode)`. A hostile vmmd lying *down* (claiming less residency than it has) cannot shrink the live accounting and force schedd to over-admit. A hostile vmmd lying *up* (claiming more residency than it has) can only make schedd under-admit on that node — safe, non-load-bearing.

This mirrors the existing trust pattern in the inverse direction: `UpdateEgressAllowlist` (vmmd.proto:72-87) is schedd→vmmd push, and the egress_drift subscriber treats a failed push as best-effort. Capacity is vmmd→schedd push, and the chooser treats a fresh report as bias and a stale/missing one as a no-op. The two directions share the same shape (push) and differ in the consequence of a bad payload (under-admit vs. policy leak), but both are bounded by an in-process invariant the receiving daemon enforces unilaterally.

### 4.4 Backwards compatibility

Single-box default-local path is unchanged:

- `compute_nodes` schema is untouched; no new migration.
- Empty `nodeCapacityTable` causes `choosePlacementLocked` to fall back to `store.ComputeNodeUsedMB` (legacy behaviour).
- `vmmd`'s publisher only starts when `[compute_node].name` is set (multi-node path). The default-local vmmd skips the loop entirely.
- The ledger floor rule is monotone: if `applyLiveCapacityMB` returns a smaller number than `store.ComputeNodeUsedMB` did for the same ledger resident, the ledger wins (the floor is canonical). The chooser never admits more than before; this slice can only reduce over-admission on a node whose live residency exceeds the stale sum.

### 4.5 Future work

- `node_signature` field on `CapacityReport`: per-host cryptographic stamp so schedd can authenticate the report's source without trusting UDS DAC. Defer until cross-host mTLS (issue #95 slice 2) ships; the floor rule is sufficient until then.
- `compute_node_capacity_reports` audit table: ops visibility into drop counters, freshness histograms, per-node staleness alerts.
- Per-node vCPU budget (replacing the box-wide `api.VCPUSlots` today). Capacity reports already carry `vcpu_busy`; the chooser ignores it today, and a future slice will read it.

### 4.6 Rejected alternatives

- **Server-streaming RPC**: forced ack frames per tick, inverted semantics. Rejected in favor of client-streaming.
- **Heartbeat piggyback**: change `Heartbeat` from unary to server-streaming and push capacity in the same stream. Rejected because the reverse-direction rationale (vmmd.proto:63-70) is load-bearing for the production trust model. Adding a capacity payload blurs the "presence-only" contract and creates two producers on one RPC.
- **Schedd-side only, no new RPC**: extend `pkg/sched/instancestats.Poller` to also write into `nodeCapacityTable`. Rejected because freshness ties to the poller's lifecycle (which runs only on schedd) and the wording "vmmd reports" calls for a producer on vmmd. The new RPC is the right shape; Option C remains a valid fallback if the publisher ever proves too heavy.

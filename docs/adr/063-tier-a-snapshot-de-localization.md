# ADR-063 · Tier A: snapshot de-localization (residual local-cache semantics)

- **Status:** **Accepted** (revised 2026-08-16)
- **Date:** 2026-08-01
- **Issue:** Phase 2 / Gate A — record the snapshot locality decision
  taken as a side effect of Tier 1's OCI storage rollout
- **Decision:** Snapshots are **per-schedd-local caches**; the
  authoritative blob lives in the shared OCI registry (ADR-054).
  Cross-node wake pulls the blob on demand; this is acceptable for
  v1.0 because the cold-boot path (ADR-005) is the slow path and
  keeps the snapshot a cache, not a truth source.

## Context

PR #457 (Tier 1) shipped OCI storage end-to-end
(ADR-054): `imaged` publishes base + per-app layers to the OCI
registry; `vmmd` reads them. The snapshot persistence path
(ADR-018 — `snapshot_written` from schedd → imaged) was untouched:
each schedd still owns its local snapshot blob (file path under
`/srv/fc/snapshots/<deployment_id>.snap`).

Phase 2 / Gate A adds a new question: when a wake lands on
schedd-A for an app whose owner is schedd-B, does schedd-A
re-snapshot locally, or pull from schedd-B's cache?

## Decision

**Per-schedd-local cache; cross-node pulls on demand from the OCI
registry.** The snapshot is a cache; cold-boot (no snapshot) is
always legal (ADR-005).

Specifically:

- Each schedd writes its own `snapshot_local_path` row when it
  snaps an instance. No sharing, no cross-schedd snapshot file
  transfer.
- A wake on the owner schedd hits the local cache (warm-restore).
- A wake on a non-owner schedd is rejected before reaching vmmd
  (`pkg/scheddgrpc` ownership guard returns `codes.FailedPrecondition`
  on mismatch). The gateway's per-node dial cache routes the
  customer's request to the owner schedd via `apps.node_id`.
- If the owner's local snapshot is gone (drained box, disk
  pressure, etc.), the owner falls back to cold-boot
  (`CreateColdBoot`); the snapshot is a cache, not a prerequisite.
- Cross-node snapshot transfer is **out of scope**. v1.0
  cross-node wakes hit the owner; v1.1 may revisit if a
  hot-standby is added.

## Consequences

- v1.0 cross-node wake latency = owner RTT + (snapshot hit) restore
  or (snapshot miss) cold-boot. Same as v1.0 single-box latency.
- Snapshot disk usage scales linearly with (apps × owners); not
  (apps × schedds). Operationally clean.
- The OCI registry is the durable source of truth for the base
  + per-app layers; snapshots remain ephemeral.
- "Snapshot exists for every owned app" becomes "snapshot exists
  for every owned app *on the owner*". Invariant §6.2-3
  ("a live snapshot OR a cold-bootable rootfs") is unchanged
  because every owned app always has a cold-bootable rootfs in
  the OCI registry.

## Rejected alternatives

- **Cross-node snapshot replication (rsync + warm standby).**
  Rejected for v1.0: doubles snapshot disk cost, doubles the
  snapshot-rebuild pipeline complexity, and the cold-boot fallback
  is already a clean ADR-005 path.
- **Centralized snapshot server.** Rejected: contradicts the
  multi-box posture; one snapshot server is one failure domain.
- **OCI as the snapshot blob (in addition to layers).** Rejected:
  Firecracker's snapshot restore is kernel-version-pinned
  (`snapshots.fc_version`) and `FC` upgrades drop them anyway
  (ADR-005). The OCI layer is the durable artefact; the snapshot
  is a per-host micro-optimisation.
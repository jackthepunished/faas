# ADR-043 · App logs producer stream (Move 4 / issue #254)

- **Status:** accepted
- **Date:** 2026-07-28
- **Decision:** Wire the producer side of `GET /v1/apps/{slug}/logs?follow=1` end-to-end via the new `schedd.StreamAppLogs` gRPC + new `vmmd.Logs` server-streaming RPC + per-instance ring buffer (`pkg/fcvm/logbuf`).
- **Why:** Issue #254 is a tier-1 ship-blocker. Hobby+ customers cannot see their function's stdout/stderr today; the dashboard's Logs tab does nothing. The Move 3 consumer side (PR #291) shipped the typed SDK decoder; the producer side is the missing piece.
- **Consequences:**
  - New `pkg/fcvm/logbuf` package: byte-bounded, line-fragmenting ring buffer with monotonic `Seq`. ~10 MB per instance, atomic eviction on overflow.
  - New `vmmd.Logs` server-streaming RPC (issue #254 acceptance #3): `Snapshot(since_seq)` for replay + `Subscribe()` for live tail on a single channel.
  - New `schedd.StreamAppLogs` RPC (added because of the §17 ownership rule: apid must not call vmmd directly — apid dials schedd, schedd fans out to vmmd). Handled on the apid side via `cmd/apid/schedd_client.go::stubScheddClient` until the production-side wiring lands in a follow-up PR.
  - `Pkg/api.LogEvent` gets an additive `InstanceID` field (`omitempty`, per ADR-016). Old SDKs ignore it.
  - New Prometheus counter `apid_logs_emitted_total{app}` (registered on every daemon per single-registry pattern; only apid increments).
  - Cone-of-silence for the gRPC client during the rollout window: `codes.Unimplemented` from the stub → apid emits `event: degraded` + `event: end`. Consumers need to handle both shapes until the production wiring is fully rolled out.
- **Rejected alternatives:**
  - **vsock for guest stdout transport.** Cleaner security story (no host-side file rotation), but guest-init changes are required. The dashboard's logs tab was already gated on the StreamAppLogs RPC; minimising the guest-side change surface is the right trade-off for a tier-1 ship-blocker.
  - **Centralised log spool (`pkg/logspool`) + per-instance pointer.** More flexible audit + retention story, but doubles the storage surface and requires a writer goroutine per ring. Out of scope per issue #254 (G6 archive-to-S3 is a separate ticket).
  - **Simple line-channel feed (no ring).** Drops frames on every consumer-disconnect. Move 4's correctness depends on replay-from-cursor, which requires a rolling buffer.
  - **One buffer per app, not per instance.** Loses per-instance granularity — the wire shape `{seq, instance, stream, line, written_at}` requires identifying which instance emitted each frame, otherwise concurrent instances' frames interleave inseparably.
  - **Put the ring in `pkg/wire`.** Issue #254's ticket text claimed a "pre-existing ring primitive in `pkg/wire`" — that was wrong. `pkg/wire` only has metrics/daemon/grpc/version. No benefit to repackaging.
  - **Counters with high-cardinality labels.** Per-instance labelling would blow past Prometheus' "tens of thousands of series" guideline. The `app` label is bounded by per-plan app quotas (Hobby=5 / Pro=25 / Scale=100) — well inside the limit.

## Cross-references

- Issue #254 (tier-1 ship-blocker).
- Spec §6 (state machine), §4.6 (two-drive rootfs), §14 (acceptance gates).
- ADR-016 (wire-shape additive).
- ADR-009 (identical inner network world — per-instance rings share the same wire shape).
- ADR-031 (per-app egress IP allowlist — same rollout pattern with `codes.Unimplemented` fallback).
- Memories: `wire-opsmetrics-single-registry`, `cross-renderer-invariant-pattern`, `apid-park-wake-not-a-vmmd-call`, `gofmt-repo-wide-gate`, `whitebox-test-file-pattern`.

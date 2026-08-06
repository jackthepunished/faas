-- filename: 00149_reserve_slot.sql
-- Slot 149 — claimed by issue #554 (Liveness probe — restart a wedged VM;
-- the Cloud-Run-parity primitive that asks "is the VM still healthy?" and
-- destroys it after N consecutive probe failures, ADR-078).
--
-- No schema change required for v1: the liveness-driven
-- RUNNING → STOPPED (reason='liveness_failed') path writes through the
-- existing `pkg/sched.Engine.transitionWithKind` audit emit
-- (engine.go:3597-3648); the next wake cold-boots because
-- `Engine.MarkSnapshotStale` is called eagerly in
-- `Engine.DestroyForLivenessFailure` before the destroy commits
-- (engine.go:1549-1554); and the per-(app, deployment) restart sliding
-- window lives in schedd memory (`pkg/sched/liveness_window.go`), not
-- on disk. The fence body is a no-op `SELECT 1;` per ADR-041 so goose
-- applies it cleanly and writes a row in goose_db_version.
--
-- If a follow-up lands `deployments.parked_reason text` for AC #3 of
-- issue #554 ("GET /v1/apps/{slug} shows parked_reason=liveness_exhausted")
-- after the in-memory window proves insufficient, claim slot 150 for
-- that migration; do NOT back-fill this fence.
--
-- Slot note: the highest open slot on the local branch is 148 — issue
-- #561's fence (migrations/00148_overage_cap_gate_index.sql). Slot 149
-- is the first free number after that.

-- +goose Up
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
-- +goose StatementBegin
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
-- +goose StatementBegin
-- +goose StatementEnd

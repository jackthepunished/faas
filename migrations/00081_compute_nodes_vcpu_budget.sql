-- filename: 00081_compute_nodes_vcpu_budget.sql
-- +goose Up
-- Per-node vCPU admission budget (Tier A2).
--
-- Today schedd's NodeLedger checks vCPU only as a box-wide gate
-- (api.VCPUSlots = 160). On a multi-box fleet that lets one node
-- admit the entire 160 vCPU budget and starve the rest. This
-- migration adds a per-row budget so each compute_node enforces
-- its own vCPU headroom parallel to the existing per-node RAM
-- ceiling (compute_nodes.admission_ceiling_mb, migration 00024).
--
-- The default is api.VCPUSlots (160) for backwards compatibility
-- with the synthetic default-local row seeded by migration 00024:
-- a single-box install sees identical behaviour because the
-- default-local row carries the legacy 160 vCPU budget.
--
-- Operator tuning: a heterogeneous fleet can set a smaller
-- budget on a smaller box (e.g. an EX44 with vpcpus=20 and
-- vcpu_budget=40 — the 8× CPUOvercommit ratio, per §1 of the
-- financial model). The bootstrap upsert in cmd/vmmd/main.go
-- is the recommended write site; the field is operator-tunable
-- post-registration via PUT /v1/compute-nodes/{id}.
--
-- Replay-safe (PR #377 / ADR-041): ADD COLUMN IF NOT EXISTS. The
-- default is a constant; a second MigrateUp is a no-op.
--
-- Backfill: every existing row gets vcpu_budget = 160 via the
-- column default. The CHECK constraint (vcpu_budget > 0) is the
-- defensive net for a future operator that tries to set zero.
-- +goose StatementBegin
ALTER TABLE compute_nodes
    ADD COLUMN IF NOT EXISTS vcpu_budget int NOT NULL DEFAULT 160
        CHECK (vcpu_budget > 0);
-- +goose StatementEnd

-- +goose Down
-- Reverse: drop the column. A row that had a custom vcpu_budget
-- loses the value on downgrade; the chooser falls back to the
-- legacy box-wide api.VCPUSlots gate (admission.go pre-Tier-A2).
-- +goose StatementBegin
ALTER TABLE compute_nodes
    DROP COLUMN IF EXISTS vcpu_budget;
-- +goose StatementEnd

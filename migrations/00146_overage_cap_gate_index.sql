-- filename: 00146_overage_cap_gate_index.sql
-- Slot 146 — claimed by issue #561 (spend cap pauses workload) and
-- its companion PR. Slot renumber from 144 → 146 during rebase onto
-- origin/main (PR #658's renumber chain landed api_keys_provenance
-- at 144 and sessions_binding at 145; 146 is the first open slot
-- after that cascade). Per ADR-041 this fence body is a no-op
-- `SELECT 1;` so goose applies it cleanly and writes a row in
-- goose_db_version.
--
-- No schema change required: accounts.overage_cap_cents already
-- exists from migrations/00054_account_credits.sql (issue #279
-- storage layer) with the CHECK constraint pinning
-- `overage_cap_cents IS NULL OR overage_cap_cents >= 0`. The
-- enforcement layer lives in pkg/sched (Engine.admitGate), pkg/api
-- (CodeAdmissionRefused), and cmd/apid (raiseOverageCap + dashboard
-- form); this migration file is just the slot fence so the migration
-- set stays contiguous 1..N per ADR-041.
--
-- Per ADR-041 (migration slot reservation convention): the fence body
-- is a no-op `SELECT 1;` inside goose's StatementBegin/End markers
-- so goose applies it cleanly and writes a row in goose_db_version.

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

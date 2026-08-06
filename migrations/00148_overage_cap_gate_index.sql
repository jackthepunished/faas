-- filename: 00148_overage_cap_gate_index.sql
-- Slot 148 — claimed by issue #561 (spend cap pauses workload) and
-- its companion PR. Slot renumber 144 → 146 → 148 during two rebase
-- cycles onto origin/main (PR #658's renumber chain landed
-- api_keys_provenance at 144, sessions_binding at 145, scan_result
-- at 147; 148 is the first open slot after the latest rebase
-- after that cascade). Per ADR-041 this fence body is a no-op
-- `SELECT 1;` so goose applies it cleanly and writes a row in
-- goose_db_version.
--
-- No schema change required: accounts.overage_cap_cents already
-- exists from migrations/00054_account_credits.sql (issue #279
-- storage layer). The enforcement layer lives in pkg/sched
-- (Engine.admitGate), pkg/api (CodeAdmissionRefused), and cmd/apid
-- (raiseOverageCap + dashboard form); this migration file is just
-- the slot fence so the migration set stays contiguous 1..N per
-- ADR-041.

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

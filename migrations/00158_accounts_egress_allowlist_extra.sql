-- filename: 00158_accounts_egress_allowlist_extra.sql
-- +goose Up
-- Add per-account additive budget on top of the plan's
-- apps.egress_allowlist cap (issue #679 / PR-B / ADR-082).
-- The plan cap (Free 0 / Hobby 0 / Pro 16 / Scale 64) stays
-- authoritative for the default case. Operators can PATCH
-- /v1/account/egress_allowlist_extra to grant a single account
-- N additional CIDR slots above the plan cap, capped at
-- api.MaxAccountEgressAllowlistExtra (1024).
--
-- Pre-PR-B behaviour: any account whose per-app cidr[] count
-- exceeded the plan cap got 400 ErrEgressAllowlistTooLong at
-- PATCH time.
-- Post-PR-B behaviour: the validator at
-- cmd/apid/handlers_ext.go:104 reads
-- (acct.Plan.EgressAllowlistMaxSize() + acct.EgressAllowlistExtra)
-- and only the plan cap is consulted when extra = 0. The
-- operator-bundle (PR-A, issue #679 / ADR-081) is a separate
-- additive axis and merges at the vmmd side; this PR's per-account
-- extra is also additive but counted at the apid validator.
--
-- One column:
--   egress_allowlist_extra  integer NOT NULL DEFAULT 0
--     CHECK (egress_allowlist_extra >= 0)
--
-- 0 = no override; the plan cap is authoritative. The DB-level
-- CHECK enforces the floor — a negative value is rejected at
-- write time. The ceiling (1024) is enforced at the apid layer
-- (api.ErrAccountEgressAllowlistExtraOutOfRange) per the ADR-050
-- "validate at edge, not in DB" practice; the DB CHECK only
-- guards against wire-bypasses that bypass the apid validator.
--
-- Replay-safe (PR #377 / ADR-041): ADD COLUMN IF NOT EXISTS +
-- a constant default = no table rewrite, no index bloat, and a
-- second MigrateUp is a no-op.
-- +goose StatementBegin
ALTER TABLE accounts
    ADD COLUMN IF NOT EXISTS egress_allowlist_extra integer NOT NULL DEFAULT 0
        CHECK (egress_allowlist_extra >= 0);
-- +goose StatementEnd

-- +goose Down
-- Reverse: drop the column. A row that had egress_allowlist_extra=N
-- loses the bit on downgrade; the validator at
-- cmd/apid/handlers_ext.go:104 falls back to the plan cap alone,
-- so a per-app PATCH that was within the plan cap stays valid;
-- any per-app PATCH that was permitted only because extra > 0
-- would now fail ErrEgressAllowlistTooLong. There is no in-DB
-- view of which apps were using the extra slots — the operator
-- audit log (account.egress_allowlist_extra_set) is the human
-- breadcrumb for this downgrade.
-- +goose StatementBegin
ALTER TABLE accounts
    DROP COLUMN IF EXISTS egress_allowlist_extra;
-- +goose StatementEnd

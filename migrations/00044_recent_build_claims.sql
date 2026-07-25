-- +goose Up
-- +goose StatementBegin

-- B2.2 (issue #196): per-account build fairness. Track the
-- account_id + claim timestamp of every builderd claim so the
-- SELECT FOR UPDATE SKIP LOCKED can prefer accounts that haven't
-- claimed recently. The window is 30 s (cfg.FairnessWindow).
--
-- Why a separate table: the `builds` row goes terminal (succeeded
-- or failed) within seconds, so reusing it for "recently claimed"
-- would force a separate retention sweep. recent_build_claims is
-- append-only and bounded by fairness_window × claim rate; a
-- periodic cleanup is out of scope for this PR (rows older than
-- the window are harmlessly skipped by the WHERE clause; the table
-- grows ~ claim-rate rows). A bounded-size GC is a Tier 3 concern.
--
-- rollback behaviour: dropping the table degrades ClaimNextQueued-
-- BuildWithFairness to "all queued" because skipped becomes empty,
-- which is the same as the non-fairness path. Safe down-migration.
create table recent_build_claims (
    account_id uuid not null references accounts(id) on delete cascade,
    claimed_at timestamptz not null default now(),
    build_id uuid not null
);
create index recent_build_claims_claimed_at_idx
    on recent_build_claims (claimed_at);
-- account_id is the lookup key for the fairness CTE; the index below
-- turns `account_id in (select ... from skipped)` into an index scan.
create index recent_build_claims_account_id_idx
    on recent_build_claims (account_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

drop table if exists recent_build_claims;

-- +goose StatementEnd

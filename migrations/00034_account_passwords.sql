-- +goose Up
-- +goose StatementBegin

-- Issue #165 / ADR-032 (PR #2 follow-up). Argon2id PHC hashes for accounts
-- that have set a password. OAuth-only accounts have no row — their identity
-- is the oauth_links row keyed by (provider, provider_subject); the absence
-- of a password row is the signal that an OAuth-only flow is required to
-- mint a session on that account.
--
-- Why a side table rather than a column on accounts:
--
--   1. Sparse data. The vast majority of accounts will be OAuth-only (Google
--      and GitHub bind a sub + email_verified=true with no password ever
--      set). A nullable text column on accounts means every SELECT * and
--      every json.Marshal of Account carries a field that's null 90%+ of
--      the time. A side table is read only when we need it.
--   2. No account write-amplification. A future "rotate the hash on every
--      login" hardening lands as `update account_passwords set hash = …,
--      updated_at = now() where account_id = …` — never touches the
--      accounts row, so the dunning / stripe / deletion fields stay on
--      a different write path and the row's HOT chain isn't churned.
--   3. Multi-provider future. A future "passkey" or "TOTP" row could live
--      in a parallel account_passkeys / account_totp table with the same
--      FK shape, instead of cluttering accounts with N nullable columns.
--
-- PHC string format: $argon2id$v=19$m=<KiB>,t=<iters>,p=<lanes>$<salt-b64>$<hash-b64>.
-- The Argon2id parameters (memory=64MiB, time=1, threads=2 per spec §11)
-- are EMBEDDED in the stored hash, not constants at verify time — that
-- way a future "bump to memory=128MiB" rollout is a no-op migration:
-- existing rows verify with the old params, new rows carry the new ones.
-- See pkg/auth/password.go for the encoder/verifier pair.
--
-- updated_at: a future "rotate hash on login" hardening (PR #2.5 follow-up)
-- reads this to decide whether to re-hash; NULL for the (already-rare)
-- case where the row was inserted by the legacy backfill path without a
-- timestamp. Defaulted at insert by every apid code path that creates
-- a row, so the NULL case is the backfill-only path.
create table if not exists account_passwords (
    account_id uuid        primary key references accounts(id) on delete cascade,
    hash       text        not null,
    updated_at timestamptz not null default now()
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

drop table if exists account_passwords;

-- +goose StatementEnd

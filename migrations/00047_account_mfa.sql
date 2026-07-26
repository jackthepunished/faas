-- +goose Up
-- +goose StatementBegin

-- IAM-2 (issue #186). TOTP MFA state on accounts. The user has
-- chosen the column shape; the side-table argument from
-- 00039_account_passwords (sparse data, single write path) is set
-- aside here because the four fields are dense on every MFA-active
-- account and the auto-flip chokepoints (plan change, card attach,
-- 2nd deploy) already write to `accounts`.
--
-- mfa_secret_encrypted stores the age-sealed TOTP base32 secret as
-- produced by pkg/auth/totp.GenerateSecret and sealed in
-- pkg/secretbox. The encryption uses the same host age key as
-- pkg/secretbox.Seal at pkg/secretbox/seal.go:55.
--
-- mfa_recovery_codes_hash is a bytea[] of SHA-256 hashes (32 bytes
-- each). SHA-256 (not Argon2id) because the codes are high-entropy
-- (10 chars base32, ~50 bits) — Argon2id cost is unjustified and
-- the sha256sum fits on the hot path. Memory cost: 10 codes *
-- 32 B = 320 B per MFA-enrolled account.
--
-- mfa_required flags the policy ("this account must enroll on next
-- login") distinct from mfa_enrolled_at (the fact). The chokepoints
-- set mfa_required=true; the customer clears it via /enroll+confirm
-- or /disable. Default false on every new account.
--
-- mfa_enrolled_at is NULL on the (now-majority) accounts that have
-- never enrolled; non-NULL = at least one successful /confirm.

alter table accounts
    add column if not exists mfa_enrolled_at         timestamptz,
    add column if not exists mfa_secret_encrypted    bytea,
    add column if not exists mfa_recovery_codes_hash bytea[],
    add column if not exists mfa_required            bool not null default false;

-- CHECK: enrolled-implies-secret-present and
-- enrolled-implies-recovery-codes-present. The reverse isn't
-- enforced: an admin can clear mfa_secret_encrypted early (e.g. to
-- invalidate an enrollment) without leaving the row in a state
-- where the customer can't enroll again.
alter table accounts
    add constraint accounts_mfa_enrolled_shape_chk
    check (
        mfa_enrolled_at is null
        or (mfa_secret_encrypted is not null
            and coalesce(array_length(mfa_recovery_codes_hash, 1), 0) > 0)
    );

-- Index supports the dashboard audit list ("which accounts are
-- required but not yet enrolled?") and the meterd-side audit
-- ("how many mfa_required-false accounts flipped this hour?").
-- Partial: only rows where policy is in effect.
create index accounts_mfa_required_pending_idx
    on accounts (id)
    where mfa_required = true and mfa_enrolled_at is null;

-- Backfill: no existing rows are touched; the four columns land
-- with NULL / false defaults. Issue #186 is opt-in by the customer
-- via the dashboard flow; we don't auto-enroll existing accounts.

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

drop index if exists accounts_mfa_required_pending_idx;
alter table accounts drop constraint if exists accounts_mfa_enrolled_shape_chk;
alter table accounts
    drop column if exists mfa_recovery_codes_hash,
    drop column if exists mfa_secret_encrypted,
    drop column if exists mfa_enrolled_at,
    drop column if exists mfa_required;

-- +goose StatementEnd

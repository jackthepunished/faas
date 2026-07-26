-- +goose Up
-- +goose StatementBegin

-- IAM-3 (issue #187 + #244 merged, ADR-036). Server-side session
-- revocation for dashboard `faas_sid` cookies. One row per dashboard
-- login; the cookie envelope carries the row's id as `sid`. Every
-- authenticated dashboard request re-validates the row.
--
-- The `last_seen_at` column continues to update post-revoke
-- (TouchSessionLastSeen is a no-op gate-wise but updates the column
-- for ops triage). Revocation authority is exclusively `revoked_at`;
-- no CHECK constraint forbids post-revoke touches. Documented in
-- ADR-036.
--
-- FK CASCADE on account_id means a customer that hits DELETE /v1/account
-- and survives the 30-day grace window has its sessions table cleaned
-- up by the same DELETE that wipes the accounts row. No additional
-- sweep required.

create table if not exists sessions (
    id           uuid primary key default gen_random_uuid(),
    account_id   uuid not null references accounts(id) on delete cascade,
    issued_ip    inet,
    issued_ua    text,
    issued_at    timestamptz not null default now(),
    last_seen_at timestamptz,
    revoked_at   timestamptz
);

-- Partial index supports ListSessions + active-row portion of
-- RevokeSession/RevokeAllSessions. GetSession is a primary-key
-- lookup against the implicit sessions_pkey.
create index if not exists sessions_active_account_idx
    on sessions (account_id, issued_at desc)
    where revoked_at is null;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

drop index if exists sessions_active_account_idx;
drop table if exists sessions;

-- +goose StatementEnd
-- +goose Up
-- +goose StatementBegin

-- Issue #165 / ADR-032 (PR #2 follow-up). Binds OAuth provider subjects to
-- accounts so the §11 anti-takeover invariant is enforceable at the database
-- level: one OAuth (provider, sub) pair maps to exactly one account, period.
--
-- Without this table, a customer who first signs up with email+password on
-- `victim@example.com` and later signs in with Google on the same email can
-- be hijacked by either an attacker who beat them to the OAuth handshake
-- (sub-first lookup solves this) or by a future "claim this email" admin
-- tool that races with an active OAuth session. The composite primary key
-- (provider, provider_subject) is the load-bearing invariant: any attempt
-- to insert a duplicate (provider, sub) hits the PK and either upserts
-- (the same account re-authenticating) or is rejected by UpsertOAuthLink
-- (a different account trying to claim).
--
-- Schema choices:
--
--   * provider: short text ("google" | "github"). NOT an enum so a future
--     provider (SAML, Apple, Microsoft) can land without a migration.
--   * provider_subject: opaque text. Google's `sub`, GitHub's numeric `id`
--     rendered as a string. No length cap — some IdPs use 64-byte JWTs as
--     subjects (WorkOS, Stytch). A 255-byte cap is checked in the apid
--     input validator, not here.
--   * email: captured at link time so the dashboard can render "this Google
--     account is bound" without a re-fetch. NOT authoritative — the IdP is.
--   * email_verified: snapshot of the provider's `email_verified` at link
--     time. Once true at link, the row stays; a future re-verification
--     flow can refresh this (see ADR-032 "Open follow-ups"). Per spec §11,
--     no session is ever minted with `email_verified=false` at link time.
--   * created_at: timestamptz, defaulted at insert; useful for ops to see
--     "when did this customer bind their Google account?".
--
-- The (account_id) index supports the future "list linked providers for
-- this account" dashboard hint (PR #2.5 follow-up); the PK already
-- supports the hot path (sub-first lookup on every OAuth callback).
create table if not exists oauth_links (
    provider         text        not null,
    provider_subject text        not null,
    account_id       uuid        not null references accounts(id) on delete cascade,
    email            text        not null,
    email_verified   bool        not null,
    created_at       timestamptz not null default now(),
    primary key (provider, provider_subject)
);
create index if not exists oauth_links_account_idx
    on oauth_links (account_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

drop index if exists oauth_links_account_idx;
drop table if exists oauth_links;

-- +goose StatementEnd

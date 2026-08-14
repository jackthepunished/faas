-- +goose Up
-- +goose StatementBegin

-- 00268_oidc_exchanged_tokens.sql — ADR-101 / issue #270 / PR-A.
--
-- Short-lived opaque bearer backing table. One row per
-- successful POST /v1/auth/oidc/exchange. The wire-side bearer
-- (fp_oidc_<48 hex>) is hashed; the row carries the OIDC
-- provenance so the audit reader can answer "which CI job
-- shipped this?" without joining against the IdP.
--
-- The 5-min TTL is the natural expiry path. The pg contract
-- is `WHERE expires_at > NOW()` (the lookup filters at
-- read time) — TTL is the operational guarantee; the FK
-- CASCADE is the contractual GDPR §17 G2 path.
--
-- Columns:
--   id          uuid        NOT NULL PK
--                 — server-minted row id. Returned to the
--                   caller in the exchange response as
--                   token_id (audit correlation).
--   account_id  uuid        NOT NULL FK accounts(id) ON DELETE CASCADE
--                 — the owning platform account. GDPR-deleted
--                   when the account goes away.
--   token_hash  bytea       NOT NULL UNIQUE
--                 — SHA-256 of the fp_oidc_<48 hex> bearer. The
--                   UNIQUE index is the hot-path lookup.
--   expires_at  timestamptz NOT NULL
--                 — the 5-min TTL stamped at exchange time.
--                   The lookup filter `WHERE expires_at > NOW()`
--                   covers the "GET on a stale row" path.
--   issuer_url  text        NOT NULL
--                 — the OIDC issuer URL (mirrors the trust
--                   policy row's issuer).
--   subject     text        NOT NULL
--                 — the JWT `sub` claim. The audit reader
--                   correlates this against the IdP's log.
--   audience    text[]      NOT NULL
--                 — the JWT `aud` claims (the pinned audiences).
--                   Mirrors the trust policy row's audience.
--   jti         text        NULL
--                 — optional JWT ID. NULL when the IdP omits
--                   the `jti` claim (most do today).
--   created_at  timestamptz NOT NULL DEFAULT now()
--                 — exchange timestamp. Distinct from
--                   expires_at so the audit reader can
--                   compute "leaked X seconds ago".
--
-- Indices:
--   oidc_exchanged_tokens_pkey (PK on id)
--   oidc_exchanged_tokens_token_hash_key (UNIQUE on token_hash)
--   oidc_exchanged_tokens_expires_at_idx (b-tree on expires_at)
--                 — supports a future background reaper
--                   (PR-D sunset) that walks `WHERE expires_at
--                   < NOW() - interval '1 day'` to free disk.
--                   The hot-path lookup uses the token_hash
--                   UNIQUE index; the expires_at index is for
--                   the bulk reaper only.
--
-- Replay-safety: every DDL uses IF NOT EXISTS so a second
-- MigrateUp is idempotent. Same convention as 00059
-- (github_installations) and 00265 (oidc_trust_policies).
--
-- Slot history: 00266 is the next free real slot at PR-A's
-- authoring time (immediately after 00265). No prior
-- reservation existed at this slot.

create table if not exists oidc_exchanged_tokens (
    id          uuid        not null primary key,
    account_id  uuid        not null
                              references accounts(id) on delete cascade,
    token_hash  bytea       not null unique,
    expires_at  timestamptz not null,
    issuer_url  text        not null,
    subject     text        not null,
    audience    text[]      not null,
    jti         text        null,
    created_at  timestamptz not null default now()
);

create index if not exists oidc_exchanged_tokens_expires_at_idx
    on oidc_exchanged_tokens (expires_at);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
drop index if exists oidc_exchanged_tokens_expires_at_idx;
drop table if exists oidc_exchanged_tokens;
-- +goose StatementEnd

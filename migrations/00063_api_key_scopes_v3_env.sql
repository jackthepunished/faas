-- +goose Up
-- +goose StatementBegin

-- 00063_api_key_scopes_v3_env.sql — issue #395 / ADR-045.
--
-- Widens api_keys_scopes_vocab_chk to admit the two new env-scoped
-- scopes (env:read / env:write). The 00046 v2 migration
-- (00046_api_key_scopes_v2.sql:75-78) closed the vocab to a closed
-- 6-string ARRAY so a customer minting an API key with any other
-- scope string is rejected at INSERT time. Adding env scopes to the
-- in-Go constants in pkg/api/apikey.go is therefore NOT sufficient —
-- the DB CHECK has to admit them before apid will let a customer
-- POST /v1/keys with env scopes attached.
--
-- Recipe: drop the old CHECK, add a new one whose ARRAY literal
-- carries the eight accepted values. <@ means every element of the
-- scopes column must be in the set; cardinality > 0 rejects a row
-- that somehow lost all its scopes (defense in depth). Mirrors
-- 00046's exact shape.
--
-- No backfill is necessary: 00046's migration already applies; any
-- legacy row's scopes are a subset of the new ARRAY.

alter table api_keys
    drop constraint if exists api_keys_scopes_vocab_chk;

alter table api_keys
    add constraint api_keys_scopes_vocab_chk
        check (scopes <@ array[
            'admin','deploy:write','secrets:read','secrets:write',
            'usage:read','apps:read','env:read','env:write'
        ]::text[]
        and cardinality(scopes) > 0);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Restore the v2 6-string vocab so a rollback preserves the closed-set
-- invariant. A row with env:read or env:write scopes that landed under
-- the new constraint would fail this constraint on rollback — same
-- posture as 00046's Down notes, customers rolling back must manually
-- re-mint keys.
alter table api_keys
    drop constraint if exists api_keys_scopes_vocab_chk;

alter table api_keys
    add constraint api_keys_scopes_vocab_chk
        check (scopes <@ array[
            'admin','deploy:write','secrets:read','secrets:write',
            'usage:read','apps:read'
        ]::text[]
        and cardinality(scopes) > 0);
-- +goose StatementEnd

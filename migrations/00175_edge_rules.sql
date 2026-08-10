-- filename: 00175_edge_rules.sql
-- +goose Up
-- +goose StatementBegin
-- Edge rules (issue #TBD / ADR-089, planned). Customer-configurable
-- resource that runs in pkg/gateway BEFORE host→app resolution. A
-- single table unifies seven per-kind surfaces (route / rewrite /
-- redirect / headers / cors / jwt / ip) into one priority-ordered
-- matcher. See docs/adr/089-edge-rules.md for the decisions; this
-- file is the schema-only half.
--
-- Slot 172 is the next free slot on origin/main (165 was taken by
-- build_provenance.framework_version). If a sibling PR grabs 172
-- first, renumber per migrations/README.md and update the companion
-- 00175_edge_rules_test.go filename + the literal UUIDs the test
-- seeds.
--
-- Replay-safe (ADR-041 / PR #377): every DDL is guarded by IF NOT
-- EXISTS / IF EXISTS so a second MigrateUp is a no-op.
--
-- Design notes:
--  * (account_id, app_id) parent FKs both CASCADE — apid's
--    deleteAccount transaction doesn't need a second explicit
--    cascade; the FK is the single source of truth for lifecycle.
--  * jsonb `action` carries the kind-tagged union; the schema CHECK
--    on `kind` is the validation tripwire. See pkg/api/dto.go for
--    the seven per-kind action shapes (RouteAction / RewriteAction /
--    RedirectAction / HeadersAction / CORSAction / JWTAction /
--    IPAction + HeaderOp).
--  * Partial indexes `WHERE enabled` keep the working set small
--    even when customers leave disabled staging rules around.
--  * text_pattern_ops on match_host serves the gateway's LIKE
--    prefix scan (the host glob "*"→"%", "*.foo.com"→"%.foo.com")
--    without seqscan when a single host has many rules.

CREATE TABLE IF NOT EXISTS edge_rules (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id    uuid NOT NULL,
    app_id        uuid NOT NULL,
    match_host    text NOT NULL,
    match_path    text NOT NULL DEFAULT '/',
    match_methods text[] NOT NULL DEFAULT '{}',
    priority      smallint NOT NULL DEFAULT 100
                  CHECK (priority BETWEEN 0 AND 10000),
    enabled       boolean NOT NULL DEFAULT true,
    kind          text NOT NULL
                  CHECK (kind IN ('route','rewrite','redirect',
                                  'headers','cors','jwt','ip')),
    action        jsonb NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE edge_rules
    ADD CONSTRAINT IF NOT EXISTS edge_rules_account_id_fkey
        FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE CASCADE;
ALTER TABLE edge_rules
    ADD CONSTRAINT IF NOT EXISTS edge_rules_app_id_fkey
        FOREIGN KEY (app_id) REFERENCES apps(id) ON DELETE CASCADE;

CREATE INDEX IF NOT EXISTS edge_rules_enabled_match_host_idx
    ON edge_rules (match_host) WHERE enabled;
CREATE INDEX IF NOT EXISTS edge_rules_app_id_enabled_idx
    ON edge_rules (app_id) WHERE enabled;
CREATE INDEX IF NOT EXISTS edge_rules_match_host_pattern_idx
    ON edge_rules (match_host text_pattern_ops) WHERE enabled;

CREATE OR REPLACE FUNCTION edge_rules_set_updated_at()
RETURNS trigger AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS edge_rules_set_updated_at_trg ON edge_rules;
CREATE TRIGGER edge_rules_set_updated_at_trg
    BEFORE UPDATE ON edge_rules
    FOR EACH ROW EXECUTE FUNCTION edge_rules_set_updated_at();
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS edge_rules CASCADE;
-- +goose StatementEnd

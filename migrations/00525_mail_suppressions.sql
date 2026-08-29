-- filename: 00525_mail_suppressions.sql
-- +goose Up
-- +goose StatementBegin

-- Issue #246 acceptance item 7 — hard-bounce + complaint
-- suppression list (ADR-115 §D.3, RFC 8058 follow-on). One row
-- per (source, provider_event_id) so the same bounce delivered
-- twice by Resend's retry policy upserts to the same row instead
-- of double-suppressing; the unique index is the dedupe key.
--
-- Why a separate table (and not just pkg/audit rows):
--   - The audit log is an append-only ledger; querying
--     "is this address currently suppressed" against it is a
--     sequential scan and grows unbounded. The decorator needs
--     a fast lookup on every outbound mail — a single-row
--     partial index on lower(email) is O(1) at expected volumes.
--   - The suppression list has its own lifecycle (TTL via
--     expires_at, operator-driven removal) that does not belong
--     on an immutable ledger.
--   - Replay safety: the bounce handler (pkg/meter/bounce_handler.go)
--     dedupes on provider_event_id; on a duplicate webhook
--     delivery Resend retries with the same svix-id and we
--     return 200-and-ignore — the unique constraint turns a
--     redelivery race into a SQLSTATE 23505 that we map to
--     ErrConflict per the pr-1000 convention.
--
-- account_id is nullable: a bounce can land before the bounce
-- handler has correlated the recipient with an account
-- (e.g. a typo'd address the customer never used). The FK
-- still cascades so deleting an account drops its rows.
--
-- reason is a closed set: 'hard_bounce' triggers dunning
-- transition via MarkDunningStep; 'complaint' suppresses but
-- does NOT transition (suspending an account because someone
-- hit "spam" is hostile); 'manual' is for operator overrides
-- (account re-enable after a false positive).
--
-- source is closed: 'resend' / 'postmark' / 'operator'.
-- 'operator' is for the rare case where an operator
-- suppresses an address manually (e.g. a deliverability
-- complaint that the provider has not yet registered).
CREATE TABLE IF NOT EXISTS mail_suppressions (
    id                uuid                     PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id        uuid                     REFERENCES accounts(id) ON DELETE CASCADE,
    email             text                     NOT NULL,
    reason            text                     NOT NULL,
    source            text                     NOT NULL,
    provider_event_id text                     NOT NULL,
    expires_at        timestamp with time zone,
    created_at        timestamp with time zone NOT NULL DEFAULT now(),

    CONSTRAINT mail_suppressions_reason_chk CHECK (reason IN ('hard_bounce','complaint','manual')),
    CONSTRAINT mail_suppressions_source_chk CHECK (source IN ('resend','postmark','operator'))
);

-- Dedupe: the same (source, provider_event_id) bounces at most
-- once. Resend's webhook retries carry the same svix-id, so this
-- is the redelivery-race guard — the second INSERT hits
-- SQLSTATE 23505 which the bounce handler maps to ErrConflict.
CREATE UNIQUE INDEX IF NOT EXISTS mail_suppressions_event_id_key
    ON mail_suppressions (source, provider_event_id);

-- Lookup path: the suppression decorator's IsMailSuppressed
-- query does WHERE lower(email) = $1 AND (expires_at IS NULL OR
-- expires_at > now()). The partial index keeps expired rows
-- out so a row that fell out of TTL does not block the lookup.
CREATE INDEX IF NOT EXISTS mail_suppressions_active_email_idx
    ON mail_suppressions (lower(email))
    WHERE expires_at IS NULL OR expires_at > now();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Forward-only: dropping the suppression list would let
-- every address we've previously bounced start receiving mail
-- again, which is the exact regression issue #246 closes.
-- Mirrors 00141_app_webhook_deliveries.sql and
-- 00062_alert_rules.sql — operator-driven downgrade preserves
-- data; the table stays.
SELECT 1;
-- +goose StatementEnd

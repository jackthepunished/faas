-- filename: 00164_gdpr_request_id.sql
-- +goose Up
-- Issue #755 / PR-5.2: add request_id column to gdpr_requests so the
-- 24h export rate-limit can be made idempotent on X-Request-Id.
--
-- Without this column, a customer who retries a flaky export request
-- inside the 24h window would get 429 export_rate_limited for what
-- is logically the same action. With it, the second call sees the
-- prior ledger row's request_id and returns the same export payload
-- (or, if the first call never completed, the second call proceeds).
--
-- Nullable: existing rows (pre-PR-5.2) have no inbound id to record
-- so a backfill would invent semantics. NULL means "no inbound id
-- was supplied" — distinct from "" which a Go zero-string would
-- produce. The handler treats NULL/"" the same way: no idempotency
-- probe, normal rate-limit check applies.
--
-- The partial unique index is the load-bearing seam: it lets a
-- single SELECT ... WHERE request_id = $1 find the original ledger
-- row without scanning the whole table. account_id is in the index
-- so an attacker who learns a request id cannot probe another
-- account's history (the predicate always ANDs account_id).

ALTER TABLE gdpr_requests
    ADD COLUMN IF NOT EXISTS request_id TEXT;

CREATE INDEX IF NOT EXISTS gdpr_requests_request_id_idx
    ON gdpr_requests (account_id, request_id)
    WHERE request_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS gdpr_requests_request_id_idx;
ALTER TABLE gdpr_requests DROP COLUMN IF EXISTS request_id;
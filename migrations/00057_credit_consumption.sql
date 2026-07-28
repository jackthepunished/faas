-- +goose Up
-- +goose StatementBegin
--
-- 00057_credit_consumption.sql — issue #279 PR-C.
--
-- Slot note: 00057 is next-free on main after slot 56 was claimed
-- by open PRs #335 and #352 (the slot-collision gate from
-- scripts/ci/check_migration_slots.sh failed both). Walking past
-- collisions at rebase time is documented in memory
-- `migration-slot-collisions-across-prs.md` and
-- `git-mv-migration-internals-untouched.md`.
--
-- The PR #337 / #279 PR-A surface only ISSUED credits. This
-- migration closes the loop: credit consumption is recorded as
-- negative deltas on credit_ledger, deduped on
-- (provider_invoice_id, credit_id) so a webhook re-fire / admin
-- endpoint replay cannot double-decrement cents_remaining.
--
-- provider_invoice_id is NULL on issuance rows (today's only
-- writer). Consumption rows always set it; the partial unique
-- index only kicks in when the column is NOT NULL, leaving
-- issuance free of the constraint.
--
-- Idempotency: ON CONFLICT (provider_invoice_id, credit_id) DO
-- NOTHING means a second reducer run for the same invoice is a
-- no-op. The reducer detects the conflict by row count and
-- returns the same ConsumeAccountCreditResult — the operator
-- sees the same "consumed_cents" / "remaining_credits_cents"
-- regardless of which call they inspect.
--
-- Mirrors the webhook-replay guard in 00048_invoices.sql
-- (UNIQUE (account_id, provider, provider_invoice_id)) — the
-- constraint IS the dedupe row, no separate dedupe table.
--
-- Money: integer cents (CLAUDE.md: never float on money).

alter table credit_ledger
  add column if not exists provider_invoice_id text;

create unique index if not exists credit_ledger_invoice_credit_idx
  on credit_ledger (provider_invoice_id, credit_id)
  where provider_invoice_id is not null;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
drop index if exists credit_ledger_invoice_credit_idx;
alter table credit_ledger drop column if exists provider_invoice_id;
-- +goose StatementEnd

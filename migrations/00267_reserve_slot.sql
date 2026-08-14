-- filename: 00267_reserve_slot.sql
-- +goose Up
-- +goose StatementBegin
-- Contiguity filler for issue #881 Phase 3 (per-consumer rate-limit
-- keying, ADR-104). Claimed on 2026-08-14 ahead of the real DDL in
-- PR-1 (apid/state action extension). The real schema widens
-- `EdgeRuleAction` jsonb with three new properties (`key_by`,
-- `jwt_claim_name`, `max_keys_per_rule`) — jsonb carries no CHECK
-- constraint today, so the DDL is purely additive and does NOT
-- require this fence; the fence exists so the cluster's slot
-- reservation is unambiguous and survives a sibling-PR claim race.
-- Body is a no-op SELECT 1; will be shadowed by PR-1's real DDL or
-- dropped in a follow-up if the cluster abandons the slot (per
-- ADR-041 reservation fence pattern).
--
-- Slot 00266 was claimed first but cross-PR precheck found that
-- ADR-101 (OIDC, worktree-adr-101-pr-a) had already published
-- 00265_oidc_trust_policies.sql + 00266_oidc_exchanged_tokens.sql
-- before our fence landed; renumbered to 00267 to clear the
-- collision per memory [[cross-pr-slot-fence-pagination-gate]].
SELECT 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
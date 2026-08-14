-- filename: 00266_reserve_slot.sql
-- +goose Up
-- +goose StatementBegin
-- Contiguity filler for issue #881 Phase 3 (per-consumer rate-limit
-- keying, ADR-104). Claimed on 2026-08-14 ahead of PR-1's DDL.
-- The real schema widens `EdgeRuleAction` jsonb with three new
-- properties (`key_by`, `jwt_claim_name`, `max_keys_per_rule`) —
-- jsonb carries no CHECK constraint today, so the DDL is purely
-- additive and does NOT require this fence; the fence exists so
-- the cluster's slot reservation is unambiguous and survives a
-- sibling-PR claim race.
--
-- Slot 00266 is co-owned by ADR-101 (OIDC exchanged_tokens) on
-- worktree-adr-101-pr-a. Per the cross-PR fence pattern
-- (ADR-041), the sibling PR takes precedence on this slot — its
-- real DDL wins on merge. Our 00267 fence is the load-bearing
-- reservation; this 00266 fence is a contiguity-only placeholder
-- that drops out when the branches rebase (per memory
-- [[pr-887-throttle-conflict-resolution]] trap 1, "don't `git rm`
-- co-fences — restore them with `git checkout HEAD --`").
SELECT 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
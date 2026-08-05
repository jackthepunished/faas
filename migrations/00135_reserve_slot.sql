-- filename: 00135_reserve_slot.sql
-- Slot fence: four sibling PRs (#540 webhook-deliveries, #651
-- deploy-scans, #653 IAM provenance, #654 per-deployment authn)
-- all touched slot 135 in their first commits; the cross-PR
-- slot gate surfaced the cluster, and this fence holds the
-- embedded set contiguous on this branch while eviction_priority
-- lives at slot 138. PR #647 (issue #475 / ADR-075) renumbered
-- 135→138 to land past the cluster.
--
-- ADR-041 (migration slot reservation convention): the fence body
-- is a no-op `select 1;` inside goose's StatementBegin/End
-- markers so goose applies it cleanly and writes a row in
-- goose_db_version. A later PR that wants slot 135 for a real
-- schema shadows this fence; the same PR drops this file via
-- `git rm` so the carved-out slot lands cleanly on main.
-- Multiple fences at the same slot from different PRs (this
-- branch's 00135_reserve_slot.sql + #651's + #654's) all use the
-- same StatementBegin/End shape so whichever side merges last
-- resolves to the same no-op content; if any two bodies differ,
-- the cross-PR slot-collision gate flags them with the same
-- overlap that originally surfaced this branch's renumber.

-- +goose Up
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
-- +goose StatementBegin
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
-- +goose StatementBegin
-- +goose StatementEnd

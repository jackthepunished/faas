-- Issue #463 / ADR-069 — PR-B sidecar runtime reservation fence.
--
-- Pin slot 00120 ahead of the sidecar work in case a sibling PR
-- claims 00119 mid-PR. After rebase lands cleanly, this fence is
-- `git rm`'d (per the cross-PR slot-collision discipline).
--
-- Slot: 00120. Latest on origin/main is 00118 (PR-A sidecars).
-- Fence discipline: migrations/README.md.

-- +goose Up
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
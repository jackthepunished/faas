-- filename: 00243_reserve_slot.sql
-- +goose Up
-- +goose StatementBegin
-- Co-fence for the issue #879 / ADR-100 tenant-surfaces PR-cluster.
-- This slot is the cluster's PR-A target: the real
-- 00243_tenant_surfaces.sql replaces this fence in PR-A. Body is a
-- no-op SELECT 1 today.
SELECT 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd

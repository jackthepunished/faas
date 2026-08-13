-- filename: 00238_reserve_slot.sql
-- +goose Up
-- +goose StatementBegin
-- Co-fence for the issue #879 / ADR-100 tenant-surfaces PR-cluster.
-- Slot 238 is between the post-00237 fence cluster and the
-- 00243_reserve_slot.sql that holds the cluster's real migration.
-- The cluster's PR-A will land at 00243_tenant_surfaces.sql; this
-- fence and the 00239-00242 siblings keep the embedded set contiguous
-- under TestMigrationsContiguous. Mirrors the cross-pr-slot-fence
-- pagination gate pattern (PR #867). Body is a no-op SELECT 1.
SELECT 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd

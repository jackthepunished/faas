-- filename: 00240_reserve_slot.sql
-- +goose Up
-- +goose StatementBegin
-- Co-fence for the issue #879 / ADR-100 tenant-surfaces PR-cluster.
-- See 00238_reserve_slot.sql for the rationale. Body is a no-op
-- SELECT 1; slot 240 is held by the cluster's PR-A.
SELECT 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd

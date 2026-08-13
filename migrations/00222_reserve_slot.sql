-- filename: 00222_reserve_slot.sql
-- +goose Up
-- +goose StatementBegin
-- Reserve slot 222 for the next PR-cluster PR after the CORS
-- improvements (e.g. dashboard UI for CORS — pkg/dashboard
-- templates + HTMX). Same fence rationale as 00211; see
-- cross-pr-slot-gate-reservation-fence-pattern.
SELECT 1;
-- +goose StatementEnd

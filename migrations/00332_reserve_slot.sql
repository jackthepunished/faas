<<<<<<< HEAD
-- 00332_reserve_slot.sql — temporary concurrent-PR migration fence.
=======
-- filename: 00332_reserve_slot.sql
>>>>>>> 15ef86115 (feat(migrations): ADR-119 apps_public_auth_mode widens to include 'internal_only' (slot 00333 + 6 bridge fences 00327-00332))
-- +goose Up
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd

-- +goose Up
-- +goose StatementBegin
ALTER TABLE accounts RENAME COLUMN stripe_customer_id TO provider_customer_id;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE accounts RENAME COLUMN provider_customer_id TO stripe_customer_id;
-- +goose StatementEnd

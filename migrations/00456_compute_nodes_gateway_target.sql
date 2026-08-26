-- +goose Up
-- +goose StatementBegin

-- Stable split-box ingress (control-plane/data-plane separation). The public
-- gateway reads this endpoint from the compute-node registry instead of a
-- manifest-rendered first-compute target. Nullable preserves legacy and
-- single-box rows; a node without this endpoint is not eligible for public
-- ingress but can still be used by the scheduler through target_url.
alter table compute_nodes
  add column if not exists gateway_target_url text;

alter table compute_nodes
  drop constraint if exists compute_nodes_gateway_target_url_scheme_chk;

alter table compute_nodes
  add constraint compute_nodes_gateway_target_url_scheme_chk
    check (gateway_target_url is null or gateway_target_url ~ '^tcp://[^/:][^/]*:[0-9]+$');

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

alter table compute_nodes
  drop constraint if exists compute_nodes_gateway_target_url_scheme_chk;
alter table compute_nodes
  drop column if exists gateway_target_url;

-- +goose StatementEnd

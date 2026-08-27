-- +goose Up
-- +goose StatementBegin

-- Placement's cold-start fallback aggregates live instances by compute node.
-- Keep that one bulk query index-backed as the fleet grows; the vmmd capacity
-- stream remains the steady-state source and this index serves startup and
-- transient stream gaps.
CREATE INDEX IF NOT EXISTS instances_live_node_id_idx
  ON instances (node_id)
  WHERE state IN ('waking', 'cold_booting', 'running');

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS instances_live_node_id_idx;

-- +goose StatementEnd

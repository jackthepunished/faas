-- Placement's cold-start fallback aggregates live instances by compute node.
-- Keep that one bulk query index-backed as the fleet grows; the vmmd capacity
-- stream remains the steady-state source and this index serves startup and
-- transient stream gaps.
create index if not exists instances_live_node_id_idx
  on instances (node_id)
  where state in ('waking', 'cold_booting', 'running');

# Operator runtime configuration

The operator console at `/operations/configuration` is the control-plane
surface for settings that used to require an SSH session and an edit to
`/etc/faas/*.env` or `apid.toml`.

## State model

Each catalogued key has a durable row in `runtime_config_entries`:

- `desired_value` is what the operator requested.
- `effective_value` is what the daemon acknowledged as live.
- `version` is an optimistic-concurrency number.
- `status` is `pending`, `applied`, `failed`, or `blocked`.

Every write also appends `runtime_config_revisions` and emits the
`runtime_config_changed` notification. Notifications are only wake-ups; apid
re-reads the table at boot and after reconnect so a dropped notification cannot
silently leave a daemon stale.

## Apply modes

`hot` settings are validated, swapped into the process snapshot, acknowledged,
and are immediately visible to new requests. Feature gates and the domain
doctor TTL use this path. HSTS uses the same hot setter and is guarded for
concurrent request reads.

`graceful`, `rolling`, and `break_glass` settings are never reported as live
just because the database write succeeded. The PATCH returns `202` with a
durable row in `runtime_config_operations`; a deployment/daemon controller
claims that row, applies the change, and calls the state-layer terminal method.
Successful completion updates both the operation and the matching
`runtime_config_entries` row in one database transaction. Failure/block reasons
remain visible to the operator.

Bootstrap secrets, listener addresses, billing provider selection, and daemon
role are deployment-managed and are intentionally not editable in the web
console. This prevents a UI action from creating a partial topology or a
credential outage.

## API

- `GET /v1/admin/config` — catalog plus desired/effective state.
- `PATCH /v1/admin/config/{key}` — hot apply or queue an asynchronous apply.
- `GET /v1/admin/config-operations/{id}` — poll an asynchronous apply.
- `GET /v1/admin/config/{key}/revisions` — inspect append-only history.

All writes require admin scope, MFA, a reason, and an optional expected
version. Sensitive values are redacted in list, operation, and revision
responses.

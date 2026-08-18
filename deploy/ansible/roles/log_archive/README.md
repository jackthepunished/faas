# `log_archive` ansible role

Operator wiring for the issue #562 log archive pipeline. Two
daemons share one credential envelope:

| daemon            | role                                  |
| ----------------- | ------------------------------------- |
| `apid`            | writes per-instance logs to the bucket (PR-A shipper) |
| `gatewayd-internal` | reads per-day archives back on `?archive=1` (PR-B handler) |

The Go-level wiring (config parsing, SigV4 sign, S3 client) lives
in `pkg/logarchive` and is loaded by both daemons via `os.Getenv`
on the same `FAAS_LOG_ARCHIVE_*` envelope. This role is the
host-level glue: where the envelope lives, what perms it must
have, and where the local spool root sits.

## What this role does

1. Fail-closed asserts on
   `/etc/faas/secrets/storage-box/archive-creds.json`
   (mode `0440`, owner `root`, group `faas` — spec §11). Both
   daemons read this via systemd's `LoadCredential=` so the
   plaintext never appears on the host filesystem outside
   the unit's `$CREDENTIALS_DIRECTORY`.
2. Creates `/var/log/faas/archive` (`faas:faas`, mode `0750`).
   The apid shipper writes `.partial` files here on the way
   to the bucket; gatewayd-internal does NOT touch this dir
   (read-back is bucket-only, not spool).
3. Drops
   `/etc/systemd/system/faas-gatewayd-internal.service.d/99-faas-log-archive.conf`
   so the read-back handler can LoadCredential the same
   envelope apid reads. Mirrors the apid drop-in at
   `control_plane_service/files/faas-apid.service.d/`.
4. Asserts the spool root mode is `≤ 0750` and the drop-in
   itself is `0644 root:root` (spec §11).
5. Runs `systemctl daemon-reload` so the next gatewayd-internal
   restart picks up the new `LoadCredential=`.

## What this role does NOT do

- **No cron purge unit.** The apid shipper's `PurgeInterval`
  ticker (`pkg/logarchive/shipper.go:125`) already sweeps the
  local spool on the same cadence the spec calls for. Adding
  a second systemd timer would race the in-process purger on
  the same files; the apid shipper is the canonical owner.
  Bucket-side retention is enforced by the customer's plan
  (Hobby 7d / Pro 30d / Scale 90d — plan-gated at the
  gatewayd-internal handler) and by the S3 lifecycle policy
  the operator attaches at bucket-provision time (see the
  runbook).
- **No `gregalectl backup unseal-archive-creds` invocation.** That
  CLI ships in PR-A and the operator runs it once during
  `bootstrap.sh` step 11d (RETIRED 2026-08-15 by issue #911 / PR-1;
  v2 path is PR-X `gregalectl secrets init`) — mirrors the `unseal-rclone`
  flow at `deploy/ansible/roles/postgres/files/postgresql.service.d/
  99-faas-storage-box.conf`. This role assumes the envelope
  is already on disk; the assert catches any future perms
  drift.
- **No daemon restart.** `daemon-reload` is fired so the new
  drop-in is picked up on the *next* `systemctl restart
  faas-gatewayd-internal` (an operator action). The role
  never silently bounces the edge on a deployed box.
- **No S3 lifecycle policy attachment.** That's an operator
  action on the bucket itself; see `docs/runbooks/
  FaasLogArchiveShipperDegraded.md` "Bucket lifecycle
  policy" section for the recommended shape.

## Idempotency

`copy` overwrites cleanly; `file` for the spool root is a
no-op on a converged box; the asserts are read-only. Re-running
on a converged host produces zero `changed`.

## Refs

- Issue #562 (acceptance matrix)
- `pkg/logarchive` (PR-A: shipper + config)
- `cmd/gatewayd-internal/app_logs_archive.go` (PR-B: read-back)
- `deploy/ansible/roles/control_plane_service/files/faas-apid.service.d/99-faas-log-archive.conf` (sibling drop-in)
- `docs/runbooks/FaasLogArchiveShipperDegraded.md` (operator runbook)

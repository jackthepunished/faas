# `postgres_backup` ansible role

Drops and enables the `faas-pg-basebackup.{service,timer}` pair (local
basebackup) + the `faas-pg-basebackup-push.{service,timer}` pair
(off-host push to Hetzner Storage Box, issue #250). Both timers run
on the 03:00 / 03:30 UTC cadence.

## What this role does

1. Fails-closed if `HETZNER_STORAGE_BOX_USER` / `HETZNER_STORAGE_BOX_HOST`
   are unset (issue #250 acceptance #3).
2. Installs `rclone` via apt (matches the postgres-15 install pattern;
   no vendored binaries).
3. Creates `/var/lib/pgsql/basebackup` (postgres-owned, postgres group,
   mode `0750`) — the drill script's `LATEST_BB` parent.
4. Copies `faas-pg-basebackup.{service,timer}` into
   `/etc/systemd/system/`.
5. Creates `/etc/faas/secrets/storage-box/` (0700 root:root).
6. Copies `faas-pg-basebackup-push.{service,timer}` into
   `/etc/systemd/system/`.
7. Runs `systemctl daemon-reload`, then enables + starts both timers.
8. Asserts `/var/lib/pgsql/basebackup` mode is `≤ 0750` (spec §11).
9. Asserts `/etc/faas/secrets/storage-box/{rclone.conf,box-age-key}`
   are 0400 root:root (issue #250 fail-closed, spec §11).

## Why no PG config changes

The `postgres` role already configures `wal_level = replica`,
`archive_mode = on`, `archive_command` (issue #250 rewrites the
local-only `cp` baseline to compound local `cp` + rclone push),
and `max_wal_senders = 3` (`roles/postgres/tasks/main.yml:118-156`).
The basebackup + push services only need those + the running
cluster — no new GUCs, no restart.

## Idempotency

`copy` overwrites cleanly; `systemd` enable+start is a no-op on a converged
box; the mode assertions are read-only. Re-running on a converged host
produces zero `changed`.

## Carve-outs

- The role does NOT run `pg_basebackup` itself — only enables the timer.
  Operators who need an immediate backup use `make backup-pg` (Makefile).
- The role does NOT push itself — only enables the push timer.
  Operators who need an immediate push use `make backup-push-pg`
  (Makefile) — same `systemctl start faas-pg-basebackup-push.service`
  under the hood.
- The role does NOT manage the timer schedule — `OnCalendar` lives in
  the unit file; editing it is a one-line `copy` change.
- The role does NOT prune old basebackups. The drill picks the newest by
  mtime; an explicit prune is M9 (`docs/drills/TEMPLATE-restore-drill.md`).

## Refs

- Spec §14 M8 row.
- Issue #250 acceptance matrix.
- `deploy/scripts/faas-m8-restore-drill.sh` (local-disk sibling drill).
- `deploy/scripts/pg-restore-verify.sh` (off-host T-7 verify).
- `docs/runbooks/PostgresBackup.md` (operator runbook).
- `docs/drills/TEMPLATE-restore-drill.md` (drill record format).

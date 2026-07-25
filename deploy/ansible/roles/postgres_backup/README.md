# `postgres_backup` ansible role

Drops and enables the `faas-pg-basebackup.{service,timer}` pair that takes
a nightly `pg_basebackup` into `/var/lib/pgsql/basebackup/` for the M8
restore-drill acceptance gate (spec §14).

## What this role does

1. Creates `/var/lib/pgsql/basebackup` (postgres-owned, postgres group, mode
   `0750`) — the drill script's `LATEST_BB` parent.
2. Copies `faas-pg-basebackup.service` into `/etc/systemd/system/`.
3. Copies `faas-pg-basebackup.timer` into `/etc/systemd/system/`.
4. Runs `systemctl daemon-reload`, then enables + starts the timer.
5. Asserts `/var/lib/pgsql/basebackup` mode is `≤ 0750` (spec §11).

## Why no PG config changes

The `postgres` role already configures `wal_level = replica`,
`archive_mode = on`, `archive_command = 'cp %p /var/lib/pgsql/archive/%f'`,
and `max_wal_senders = 3` (`roles/postgres/tasks/main.yml:118-156`). The
`pg_basebackup` call in the service unit only needs those + the running
cluster — no new GUCs, no restart.

## Idempotency

`copy` overwrites cleanly; `systemd` enable+start is a no-op on a converged
box; the mode assertion is read-only. Re-running on a converged host
produces zero `changed`.

## Carve-outs

- The role does NOT run `pg_basebackup` itself — only enables the timer.
  Operators who need an immediate backup use `make backup-pg` (Makefile).
- The role does NOT manage the timer schedule — `OnCalendar` lives in
  the unit file; editing it is a one-line `copy` change.
- The role does NOT prune old basebackups. The drill picks the newest by
  mtime; an explicit prune is M9 (`docs/drills/TEMPLATE-restore-drill.md`).

## Refs

- Spec §14 M8 row.
- `deploy/scripts/faas-m8-restore-drill.sh` (consumer).
- `docs/drills/TEMPLATE-restore-drill.md` (drill record format).

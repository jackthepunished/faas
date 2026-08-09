# ADR-056 · Off-host Postgres backup to Hetzner Storage Box

> **Slot-collision note (2026-08-09):** The slot number 056 is shared with
> [`docs/adr/056-wire-node-verifier.md`](056-wire-node-verifier.md)
> (handshake-layer CN-binding verifier, accepted 2026-08-09 alongside this
> ADR). Per the repo-wide convention recorded in
> [`docs/adr/068-issue-517-closure-evidence.md`](068-issue-517-closure-evidence.md)
> §"Note on slot-collision hygiene", the **filename is canonical** —
> readers following a "ADR-056" citation should pick the file whose
> subject matches the citation. ADR-025 §Tier 2 pre-requisites and the
> `PostgresBackup.md` runbook cite this file by name.

- **Status:** accepted v1.0
- **Date:** 2026-07-31
- **Accepted:** 2026-08-09
- **Decision:** Ship a continuous WAL push (via a rewritten
  `archive_command`) + a nightly basebackup push (via a new
  `faas-pg-basebackup-push.{service,timer}` pair) to a Hetzner
  Storage Box over SFTP (rclone). The Storage Box holds both the
  WAL stream and the basebackup tree; a throwaway restore-verify
  script (`deploy/scripts/pg-restore-verify.sh`) closes the loop
  with row-count assertions on `accounts` / `apps` / `instances`.
  Credentials (`rclone.conf` + `box-age-key`) live under
  `/etc/faas/secrets/storage-box/` (0700 root:root), sealed at rest
  via the platform's existing `host.age` primitive (PR #371 /
  ADR-040), and surfaced at runtime via systemd `LoadCredential=`
  — the same pattern as `faas_session_key` (apid) +
  `faas_host_age_identity` (apid).

## Why

Today the M8 restore drill (`deploy/scripts/faas-m8-restore-drill.sh`)
proves a local-disk round-trip works. It does NOT protect against a
host loss: a wiped `/var/lib/pgsql/data/` + `/var/lib/pgsql/archive/`
+ `/var/lib/pgsql/basebackup/` (the spec's "S3 of one node" scenario)
is unrecoverable. Spec §14 M8 row is explicit on the off-host half:
> backup drill (PG + one app back serving on a clean VM < 30 min,
> documented as executed)

Tier 1 Phases 1-5 (PRs #445, #457, #468, #469) closed the
data-plane + wire mTLS gap. The remaining hole for production-tier
multi-node is the database: a wire-bound cluster that can't recover
its control-plane state on a CP-host loss is not production-grade.

The user's three approved choices close the gap:

1. **Sealed-at-rest rclone.conf + box-age-key** under
   `/etc/faas/secrets/storage-box/` (matches the existing
   `host.age` precedent — no new key-management primitive).
2. **T-7 throwaway restore-verify** on the same host, isolated
   `PG_DATA` under `/var/lib/pgsql/restore-test/`, isolated port
   5433. Same isolation guarantee as a guest VM, cheaper than
   booting metal for a verify run.
3. **Nightly basebackup + continuous WAL archive** — the local
   archive stays (the M8 drill still consumes it for the
   fast-replay path); the rclone push is additive.

## What we chose NOT to do

- **pgBackRest / WAL-G orchestration** — out of scope until basic
  push is stable. The current "cp WAL locally + rclone to box"
  is the cheapest viable design (matches the M8 baseline's cp-only
  archive_command, just adds an off-host replica).
- **Multi-region replication** — `gate-a.md` runbook is the
  authoritative spec for active-passive. Postgres on a single
  Storage Box is the first step; multi-region is M9.
- **Hetzner-managed Postgres** — spec says "Postgres on the node";
  managed Postgres is a different architecture.
- **Per-customer WAL envelope encryption** — the entire archive
  is sealed at rest by the Storage Box's SFTP/SSH channel + the
  `host.age`-sealed rclone.conf. Per-segment envelope encryption
  is M9 (the `app_secrets` table's `host.age` pattern is the
  established primitive when we get there).
- **Object-lock / WORM retention** — operator-enabled on the
  Storage Box side; defer until basic push is stable.
- **`LoadCredentialEncrypted=`** — requires systemd 256+ + a
  TPM-bound key. Our threat model is sealed-at-rest via age, so
  plain `LoadCredential=` (plaintext at runtime, sealed at rest)
  is the right primitive.

## Load-bearing design choices

### 1. Compound `archive_command` (local `cp` + rclone `&&`)

```sh
test ! -f /var/lib/pgsql/archive/%f && \
  cp %p /var/lib/pgsql/archive/%f && \
  rclone copyto %p hertznerbox:${hetzner_storage_box_wal_path}/%f \
    --config=%d/faas_storage_box_rclone --stats=0 --quiet
```

The `&&` chain is critical: a single rclone failure must NOT
block WAL recycling — that would fill `pg_wal/` and take the
cluster down. Compound `&&` keeps the local archive on the M8
fast-replay path AND ships the off-host replica asynchronously.

The `test ! -f` guard prevents re-archival on restart
(`archive_command` re-runs after a `pg_ctl reload` for any
WAL whose segment was recycled).

### 2. Push timer pair, not embedded in the basebackup timer

The push is `rclone sync` on `/var/lib/pgsql/basebackup/` —
disk-bound + network-bound, potentially multi-GB. Bundling it
into the basebackup timer would couple two failure modes
(disk-write to basebackup + disk+network to box). Splitting
gives:

- `faas-pg-basebackup.{service,timer}` — 03:00 UTC, 30 min
  timeout, owned by postgres user.
- `faas-pg-basebackup-push.{service,timer}` — 03:30 UTC, 2h
  timeout, owned by root (needs to read
  `/etc/faas/secrets/storage-box/rclone.conf`).

The 30-min slack is generous on a quiet box (pg_basebackup
typically takes < 5 min) and bounded on a busy one.

### 3. Credentials under `/etc/faas/secrets/storage-box/`

Same shape as the existing `host.age` precedent
(`deploy/ansible/roles/control_plane_service/tasks/main.yml:108-209`):

- `rclone.conf` — 0400 root:root — the `[hertznerbox]` block
  + `host = u123456@u123456.your-storagebox.de`.
- `box-age-key` — 0400 root:root — sealed-at-rest age identity,
  decrypted into the staging dir at ansible-run time.

The fail-closed assert in `postgres_backup/tasks/main.yml` is
the canonical `stat` + `failed_when` block on mode + owner +
group. A wrong mode refuses to deploy the role rather than
silently shipping a world-readable credential.

### 4. Throwaway verify on port 5433

`deploy/scripts/pg-restore-verify.sh` uses
`/var/lib/pgsql/restore-test/data` (NOT `/var/lib/pgsql/data`)
+ ephemeral `port = 5433`. The host-only design shares the
kernel + cgroup tree with the live cluster, but never touches
the live cluster's data dir. A corrupted verify run leaves
the live cluster unaffected; a `pg_ctl stop` + `rm -rf` resets
the test surface for the next run.

`ROW_COUNT_THRESHOLD=0.95` is well above noise (live cluster
writes a few seconds between rclone copy + the count(*)) and
well below a partial-restore (which lands at 0% for a
truncated WAL stream).

### 5. Gauge on apid, not meterd

`pg_backup_last_pushed_seconds` lives on apid's OpsMetrics
because it's a cluster-wide gauge (one CP host, one
basebackup root). The sampler is a 60s tick — fast enough for
the `PgBackupStale` alert's `for: 5m` window (always at least
5 fresh ticks per evaluation), slow enough to avoid burning
IO.

Pre-instantiated to 0 on boot (same precedent as
`alertEvaluatorEnabled` in `pkg/wire/metrics.go:771-779`).
Without this, a freshly-booted box looks identical to one
with no basebackup root — both return NaN to the alert query,
and the alert is silently skipped.

## File summary

- `deploy/systemd/faas-pg-basebackup-push.{service,timer}` —
  NEW unit pair.
- `deploy/ansible/roles/postgres_backup/tasks/main.yml` —
  MODIFY: install rclone, drop new units, fail-closed asserts.
- `deploy/ansible/roles/postgres/tasks/main.yml` —
  MODIFY: rewrite `archive_command` to compound `cp && rclone`;
  install `postgresql.service.d/99-faas-storage-box.conf` drop-in.
- `deploy/ansible/roles/postgres/files/postgresql.service.d/99-faas-storage-box.conf` —
  NEW: LoadCredential + Environment for the postgresql unit.
- `deploy/ansible/roles/postgres_backup/README.md` —
  MODIFY: document off-host push.
- `deploy/ansible/roles/prometheus/{tasks/main.yml,files/pg_backup.rules.yml,templates/prometheus.yml.j2}` —
  MODIFY + NEW: drop `pg_backup.rules.yml` and reference it
  in `rule_files`.
- `deploy/ansible/group_vars/box/hetzner.yml` —
  NEW: storage-box env var wiring.
- `deploy/ansible/site.yml` —
  MODIFY: load `group_vars/box/hetzner.yml`.
- `deploy/controlplane/sealed.env.example` —
  MODIFY: document `HETZNER_STORAGE_BOX_*` vars.
- `deploy/scripts/pg-restore-verify.sh` +
  `deploy/scripts/pg-restore-verify_test.sh` —
  NEW: T-7 throwaway verify + bash lint.
- `docs/runbooks/PostgresBackup.md` —
  NEW: seven-section operator runbook.
- `Makefile` —
  MODIFY: `backup-push-pg`, `backup-restore-verify`,
  `lint-pg-restore-verify`.
- `pkg/wire/metrics.go` +
  `pkg/wire/metrics_test.go` —
  MODIFY + ADD TEST: `pg_backup_last_pushed_seconds` gauge.
- `cmd/apid/main.go` +
  `cmd/apid/pg_backup_pushed_sampler.go` +
  `cmd/apid/pg_backup_pushed_sampler_test.go` —
  MODIFY + NEW: 60s sampler goroutine.

## Refs

- Spec §14 M8 row.
- Issue #250 acceptance matrix.
- `docs/runbooks/multi-host-rollout.md` — Tier 1 status; this
  ADR closes the last "staging-only" bullet on Tier 1.
- `docs/runbooks/PostgresBackup.md` — operator runbook.
- PR #371 (cosign keypair operator CLI) — `host.age`-sealed
  secret precedent.
- PR #445 (cross-host mTLS) — Tier 1 Phase 1.
- PR #457 (multi-box bundle) — Tier 1 Phases 2-3.
- PR #468 (per-host egress) — Tier 1 Phase 4.
- PR #469 (NodeVerifier handshake layer) — Tier 1 Phase 5.

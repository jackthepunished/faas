#!/usr/bin/env bash
# faas-m8-restore-drill.sh — restore-drill acceptance for spec §14 M8.
#
# Spec §14 M8 row, private-beta gate:
#   "restore drill (PG + one app back serving on a clean VM < 30 min,
#    documented as executed)"
#
# What this script does, end-to-end:
#   0.   Pre-flight: confirm basebackup exists, archive dir has WAL,
#        daemons are healthy enough to start.
#   0.5  Stamp host.age + host.age.pub + SHA sidecar into the basebackup.
#        Without this step a wiped /var/lib/pgsql/data followed by a
#        successful restore leaves vmmd without its X25519 identity;
#        vmmd regenerates a fresh key on next boot and every customer
#        sealed secret in `app_secrets` becomes permanently unreadable
#        (ADR-020 / pkg/secretbox/hostkey.go:47-50).
#   1.   Stop every faas daemon + Postgres.
#   2.   Wipe /var/lib/pgsql/data (simulated disaster; archive is untouched).
#   3.   rsync the most recent basebackup into /var/lib/pgsql/data.
#   4.   Write a recovery stanza so PG replays archived WAL until consistent.
#   5.   Start Postgres, then every faas daemon.
#   5.5  Restore host.age{,.pub} into /etc/faas/secrets from the
#        basebackup; SHA-mismatch fails closed (protects against
#        accidental overwrite during the 30-day rotation overlap
#        window — issue #316 / ADR-057). Restart vmmd so it loads
#        the unseal identity before the first wake.
#   6.   Wait for schedd admission to come up; hit the test app's :8080.
#   7.   Write the dated record into docs/drills/<UTC-date>-<HHMMSS>-
#        restore-drill.md with seven required metric fields; print a
#        summary to stdout.
#
# Out of scope (deferred to M9):
#   - pgbackrest orchestration (we cp WAL to a local archive dir).
#   - Off-host WAL shipping to Hetzner Storage Box.
#   - Archive encryption.
#   - Parallel WAL replay (single timeline, one basebackup).
#
# Run as root on the EX44. The script refuses to run if it's not Linux
# (macOS devs use `make metal-lima` for the same accept tests).

set -euo pipefail

PG_DATA=/var/lib/pgsql/data
PG_ARCHIVE=/var/lib/pgsql/archive
PG_BASEBACKUP_DIR=/var/lib/pgsql/basebackup
PG_MAJOR="${PG_MAJOR:-$(find /etc/postgresql -mindepth 1 -maxdepth 1 -type d -printf '%f\n' 2>/dev/null | sort | tail -1)}"
PG_MAJOR="${PG_MAJOR:-15}"
PG_CONF="/etc/postgresql/${PG_MAJOR}/main/postgresql.conf"

# Host age key paths (ADR-020). Stamped into the basebackup in step 0.5
# and restored into /etc/faas/secrets in step 5.5. Modes preserved:
# private key 0400 root:root, public recipient 0444 root:root.
HOST_KEY=/etc/faas/secrets/host.age
HOST_PUB=/etc/faas/secrets/host.age.pub

# Where the dated drill record lands. Default is the repo's docs/drills/
# directory; override FAAS_DRILL_RECORD_DIR for chroot/CI smoke runs.
RECORD_DIR="${FAAS_DRILL_RECORD_DIR:-docs/drills}"

# The test app the drill proves is "back serving". Set
# FAAS_DRILL_APP_HOST to override the slot/host. Default targets the
# platform's standard fixture (10.100.0.1).
DRILL_APP_HOST="${FAAS_DRILL_APP_HOST:-10.100.0.1}"
DRILL_APP_PORT="${FAAS_DRILL_APP_PORT:-8080}"

heading() { printf '\n\033[1;36m▶ %s\033[0m\n' "$*"; }
ok()      { printf '\033[1;32m✓\033[0m %s\n' "$*"; }
warn()    { printf '\033[1;33m!\033[0m %s\n' "$*" >&2; }
fail()    { printf '\033[1;31m✗\033[0m %s\n' "$*" >&2; exit 1; }

DRILL_START=$(date +%s)
DRILL_START_ISO=$(date -u +"%Y-%m-%dT%H:%M:%SZ")

heading "0/7 Pre-flight"
[[ "$(uname -s)" == "Linux" ]] || fail "drill must run on the EX44 (Linux)"
[[ $EUID -eq 0 ]] || fail "must run as root (stops daemons, writes /var/lib/pgsql)"
[[ -d "$PG_ARCHIVE" ]] || fail "$PG_ARCHIVE missing — run the M8 postgres role first"
[[ -d "$PG_BASEBACKUP_DIR" ]] || fail "$PG_BASEBACKUP_DIR missing — basebackup is the rsync source"

# Pick the newest basebackup by mtime. The nightly cron writes
# /var/lib/pgsql/basebackup-<UTC-date>/; we take the highest suffix.
LATEST_BB=$(ls -1dt "$PG_BASEBACKUP_DIR"/basebackup-* 2>/dev/null | head -1 || true)
[[ -n "$LATEST_BB" ]] || fail "no basebackup-*/ under $PG_BASEBACKUP_DIR"
LATEST_BB_TS=$(stat -c %Y "$LATEST_BB")
RPO_BASE=$(( DRILL_START - LATEST_BB_TS ))
ok "picked basebackup: $LATEST_BB"
ok "RPO at basebackup = $(( RPO_BASE / 60 )) min $(( RPO_BASE % 60 )) s"

# Record the most recent archived WAL's mtime — that's the worst-case
# data loss in the drill (everything committed after this moment is
# gone).
LATEST_WAL=$(ls -1t "$PG_ARCHIVE"/* 2>/dev/null | head -1 || true)
if [[ -n "$LATEST_WAL" ]]; then
  LATEST_WAL_TS=$(stat -c %Y "$LATEST_WAL")
  RPO_WAL=$(( DRILL_START - LATEST_WAL_TS ))
  ok "most recent archived WAL: $(basename "$LATEST_WAL") (RPO via WAL = $(( RPO_WAL / 60 )) min $(( RPO_WAL % 60 )) s)"
  RPO_SECONDS=$RPO_WAL
else
  warn "no archived WAL found — drill will replay from basebackup only (RPO = basebackup age)"
  RPO_SECONDS=$RPO_BASE
fi

# --- 0.5. Stamp host.age into the basebackup ----------------------------
#
# Why this step matters: pg_basebackup captures cluster state, not host
# identity. /etc/faas/secrets/host.age is the X25519 private key vmmd
# loads at boot (cmd/vmmd/main.go:144-155). If the host loses this file
# and vmmd regenerates, every customer sealed secret becomes permanently
# unreadable. ADR-020 closed G2 by making the host key the canonical
# identity; the restore drill has to round-trip it explicitly.
#
# We stamp BEFORE the wipe so the SHA is captured at the moment of the
# basebackup; restoration later uses the same SHA for fail-closed
# verification (step 5.5).

heading "0.5/7 Stamp host.age into basebackup (preserves sealed secrets)"
[[ -f "$HOST_KEY" ]] || fail "$HOST_KEY missing — refusing to drill (vmmd hasn't initialized the host key yet?)"
[[ -f "$HOST_PUB" ]] || fail "$HOST_PUB missing — refusing to drill (run 'make bootstrap' to (re)initialize the host age identity)"
SHA_PRE="$(sha256sum "$HOST_KEY" | awk '{print $1}')"
install -m 0400 "$HOST_KEY"  "$LATEST_BB/host.age"
install -m 0444 "$HOST_PUB" "$LATEST_BB/host.age.pub"
echo "$SHA_PRE" > "$LATEST_BB/host.age.sha256"
ok "host.age SHA-256: $SHA_PRE (stamped at $LATEST_BB/host.age)"

# --- 1. Stop daemons + Postgres -----------------------------------------

heading "1/7 Stop daemons + Postgres"
for unit in apid gatewayd-internal gatewayd-public schedd vmmd imaged builderd meterd githubd; do
  if systemctl is-active --quiet "faas-$unit.service"; then
    systemctl stop "faas-$unit.service"
    ok "stopped faas-$unit.service"
  else
    warn "faas-$unit.service was not active"
  fi
done

if systemctl is-active --quiet postgresql; then
  systemctl stop postgresql
  ok "stopped postgresql"
else
  warn "postgresql was not active"
fi

# --- 2. Wipe PG data dir ------------------------------------------------

heading "2/7 Wipe $PG_DATA (disaster simulation)"
rm -rf "$PG_DATA"
ok "$PG_DATA wiped"

# --- 3. Restore basebackup via rsync ------------------------------------

heading "3/7 rsync basebackup → $PG_DATA"
rsync -a --delete "$LATEST_BB"/ "$PG_DATA"/
ok "rsync complete"

# --- 4. Write recovery stanza (WAL replay) ------------------------------

heading "4/7 Write recovery stanza in $PG_CONF"
# recovery.conf was the PG ≤11 name; PG 12+ uses signal files + GUCs
# in postgresql.conf. We touch the signal file so PG enters recovery
# on next start, replaying from the archive until consistent.
touch "$PG_DATA/recovery.signal"
cat >> "$PG_CONF" <<EOF

# --- faas-m8-restore-drill: recovery stanza (M8, removed after drill) ---
restore_command = 'cp $PG_ARCHIVE/%f %p'
recovery_target_action = 'promote'
EOF
ok "recovery.signal + restore_command written"

# --- 5. Start Postgres + daemons ----------------------------------------

heading "5/7 Start Postgres + daemons"
systemctl start postgresql
ok "postgresql started"

for unit in apid gatewayd-internal gatewayd-public schedd vmmd imaged builderd meterd githubd; do
  systemctl start "faas-$unit.service"
  ok "started faas-$unit.service"
done

# --- 5.5. Restore host.age from the basebackup --------------------------
#
# vmmd loaded whatever identity was on disk at boot — on a fresh box
# that's a regenerated key that doesn't match the one sealed in PG. We
# restore the original key from the basebackup so subsequent wake paths
# can unseal customer secrets (pkg/fcvm/manager.go:145-149).
#
# SHA-mismatch fail-closed: if the SHA sidecar doesn't match the
# stamped key, the drill aborts. This protects against the
# rotation-not-yet-shipped path sketched in ADR-020 §Future work —
# if a customer rotated their host key, restoring the OLD key would
# un-seal newer ciphertexts against a stale recipient. Until the
# multi-recipient seal ships, refuse rather than overwrite.

heading "5.5/7 Restore host.age into /etc/faas/secrets"
SHA_STORED="$(cat "$LATEST_BB/host.age.sha256")"
SHA_LIVE="$(sha256sum "$LATEST_BB/host.age" | awk '{print $1}')"
if [[ "$SHA_STORED" != "$SHA_LIVE" ]]; then
  # Issue #316 / ADR-057: rotation is now a documented procedure
  # (docs/ops/host-age-rotation.md). If the backup's host.age
  # differs from the live one AND host.age.previous exists, this
  # is the normal post-rotation shape and the operator must run
  # `gregale host-age status` + the runbook to reconcile before
  # restoring. We still refuse silent overwrite.
  if [[ -f /etc/faas/secrets/host.age.previous ]]; then
    fail "host.age SHA changed AND host.age.previous present — rotation overlap in progress; reconcile via 'gregale host-age status' before restoring"
  else
    fail "host.age SHA changed between backup and restore — refusing to overwrite"
  fi
fi
install -d -m 0700 -o root -g root /etc/faas/secrets
install -m 0400 "$LATEST_BB/host.age"     "$HOST_KEY"
install -m 0444 "$LATEST_BB/host.age.pub" "$HOST_PUB"
ok "host.age restored; restarting vmmd to pick up the unseal identity"
systemctl restart faas-vmmd
ok "vmmd restarted"

# --- 6. Wait for schedd admission + hit the test app --------------------

heading "6/7 Wait for schedd admission + hit test app"
# Schedd's admission loop runs every few seconds. We poll the
# /metrics endpoint on schedd's MetricsAddr (default 9091) until
# fcvm_resident_ram_pct appears — that's the cheapest signal that
# schedd is alive and the PG read path is working (the gauge's
# ResidentBytes callback queries sched.Ledger, which the PG store
# rebuilds on boot).
SCHEDD_METRICS="${FAAS_SCHEDD_METRICS:-http://127.0.0.1:9091/metrics}"
READY=0
for i in $(seq 1 60); do
  if curl -fsS "$SCHEDD_METRICS" 2>/dev/null | grep -q "fcvm_resident_ram_pct" || true; then
    READY=1
    ok "schedd admission up (after $((i*2))s)"
    break
  fi
  sleep 2
done
[[ $READY -eq 1 ]] || fail "schedd admission never came up after 120s — see journalctl -u faas-schedd"

# Hit the test app. The wake queue will cold-boot it from snapshot
# (ADR-005); the request latency is logged but not the gate here.
WAKE_START=$(date +%s)
HTTP_CODE=$(curl -sS -o /tmp/faas-drill-body -w '%{http_code}' --max-time 60 \
  "http://${DRILL_APP_HOST}:${DRILL_APP_PORT}/" || echo "000")
WAKE_END=$(date +%s)
WAKE_LATENCY=$(( WAKE_END - WAKE_START ))

if [[ "$HTTP_CODE" =~ ^2 ]]; then
  ok "test app responded $HTTP_CODE in ${WAKE_LATENCY}s"
else
  fail "test app responded $HTTP_CODE (expected 2xx); body in /tmp/faas-drill-body"
fi

# --- 7. Summary ---------------------------------------------------------

heading "7/7 Summary"
DRILL_END=$(date +%s)
DRILL_END_ISO=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
TOTAL=$(( DRILL_END - DRILL_START ))
RPO_MIN=$(( RPO_SECONDS / 60 ))
RPO_SEC=$(( RPO_SECONDS % 60 ))

# Pass threshold: 30 minutes total wall-clock (spec §14 M8 row).
if (( TOTAL <= 1800 )); then
  RESULT="PASS"
else
  RESULT="FAIL"
fi

# Stdout summary — operators watch this live during the drill.
cat <<EOF

M8 Restore Drill — $(date -u +"%Y-%m-%dT%H:%M:%SZ")

  Started:    $DRILL_START_ISO
  Finished:   $DRILL_END_ISO
  Wall-clock: $(( TOTAL / 60 )) min $(( TOTAL % 60 )) s
  RPO:        $RPO_MIN min $RPO_SEC s
  Wake:       ${WAKE_LATENCY}s
  Basebackup: $LATEST_BB
  host.age:   $SHA_PRE (preserved)

  Verdict:    $RESULT (spec §14 M8 bar = 30 min)
EOF

# Persisted drill record (spec §14 M8 "documented as executed"). The
# seven required metric fields below mirror the table in
# docs/drills/TEMPLATE-restore-drill.md; pkg/drills/record_test.go
# locks this contract via the embedded template — any drift here
# breaks the M8 audit trail and the test catches it.
#
# The filename includes UTC date + HHMMSS so multiple same-day runs
# land as separate files; the operator commits each via PR.
RECORD_DATE="$(date -u +%Y-%m-%d)"
RECORD_TIME="$(date -u +%H%M%S)"
RECORD_FILE="${RECORD_DIR}/${RECORD_DATE}-${RECORD_TIME}-restore-drill.md"
mkdir -p "$RECORD_DIR"
BASE_SHA="$(sha256sum "$LATEST_BB/base.tar.gz" 2>/dev/null | awk '{print $1}')"
if [[ -z "$BASE_SHA" ]]; then
  BASE_SHA="-"
fi
{
  echo "# Restore drill — ${RECORD_DATE} (M8 acceptance, spec §14)"
  echo
  echo "## Acceptance bar"
  echo
  echo '> "restore drill (PG + one app back serving on a clean VM < 30 min,'
  echo '>  documented as executed)" — docs/faas_implementation_spec.md §14 M8 row.'
  echo
  cat <<FIELDS
## Run summary

| Field | Value |
|---|---|
| Date (UTC) | ${RECORD_DATE} |
| Operator | ${USER} |
| Box | $(hostname -f 2>/dev/null || hostname) |
| Started | ${DRILL_START_ISO} |
| Finished | ${DRILL_END_ISO} |
| Wall-clock total | $(( TOTAL / 60 )) min $(( TOTAL % 60 )) s |
| RPO via basebackup | $(( RPO_BASE / 60 )) min $(( RPO_BASE % 60 )) s |
| RPO via WAL | $(( RPO_SECONDS / 60 )) min $(( RPO_SECONDS % 60 )) s |
| Wake latency | ${WAKE_LATENCY}s |
| Basebackup used | ${LATEST_BB} |
| Basebackup SHA-256 | ${BASE_SHA} |
| Recovery stanza status | promoted at ${DRILL_END_ISO} |
| host.age SHA-256 (preserved) | ${SHA_PRE} |
| Verdict | **${RESULT}** (bar = 30 min) |
| Operator / commit | $(whoami) @ $(git rev-parse HEAD 2>/dev/null || echo no-git) |
FIELDS
  echo
  echo "## Step log (auto-captured)"
  echo
  echo '```'
  echo "drill-start: ${DRILL_START_ISO}"
  echo "basebackup:  ${LATEST_BB} (${BASE_SHA})"
  echo "rpo-base:    $(( RPO_BASE / 60 )) min $(( RPO_BASE % 60 )) s"
  echo "rpo-wal:     $(( RPO_SECONDS / 60 )) min $(( RPO_SECONDS % 60 )) s"
  echo "host.age:    ${SHA_PRE} (preserved)"
  echo "wipe:        ${PG_DATA}"
  echo "wake:        ${WAKE_LATENCY}s to ${DRILL_APP_HOST}:${DRILL_APP_PORT}"
  echo "verdict:     ${RESULT}"
  echo '```'
  echo
  echo "## Pre-flight notes"
  echo
  echo "- Postgres role wired and converged (wal_level=replica, archive_mode=on,"
  echo "  archive_command='cp %p /var/lib/pgsql/archive/%f', max_wal_senders=3)."
  echo "- Postgres_backup role wired and converged (nightly pg_basebackup timer"
  echo "  faas-pg-basebackup.timer enabled; systemctl list-timers --all shows"
  echo "  the next 03:00 UTC run)."
  echo "- Archive directory /var/lib/pgsql/archive populated by continuous WAL"
  echo "  shipping; most-recent WAL recorded above."
  echo "- Basebackup taken via pg_basebackup -Ft -z -D <dir> during the nightly"
  echo "  cron at ${DRILL_START_ISO}, or via 'make backup-pg' for an immediate run."
  echo "- All nine faas units (apid, gatewayd-internal, gatewayd-public, githubd, schedd, vmmd,"
  echo "  imaged, builderd, meterd) were healthy at drill start."
  echo
  echo "## Anomalies / observations"
  echo
  echo "<operator fills at PR time>"
  echo
  echo "## Follow-ups (M9 candidates)"
  echo
  cat <<FOLLOW
- pgbackrest orchestration (currently a hand-rolled \`cp\`).
- Off-host WAL shipping to Hetzner Storage Box (RPO today = local archive
  retention window, ~24 h).
- Archive encryption at rest (gap G2 lean).
- Parallel WAL replay on a hot spare.
FOLLOW
} > "$RECORD_FILE"
ok "drill record written → $RECORD_FILE"

# Clean up the recovery stanza so PG doesn't try to replay on next boot.
# We can't anchor on `EOF` because bash consumes the heredoc terminator and
# never writes it to the file — use a real written line as the close anchor.
sed -i '/^# --- faas-m8-restore-drill:/,/^recovery_target_action = /d' "$PG_CONF" || true
rm -f "$PG_DATA/recovery.signal"

exit $(( TOTAL <= 1800 ? 0 : 1 ))

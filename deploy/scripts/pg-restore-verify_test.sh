#!/usr/bin/env bash
# Smoke unit for the off-host Postgres restore-verify script. Asserts
# syntax + content-token presence in deploy/scripts/pg-restore-verify.sh
# (issue #250). Catches three regression classes:
#
#   1. Syntax breakage — `bash -n` without executing the script.
#   2. Required env-var / table-name drift — the table-list heredoc
#      and the rclone / RestoreTestRoot / ROW_COUNT_THRESHOLD knobs
#      must remain present in the script body. The same tokens are
#      pinned in the runbook (docs/runbooks/PostgresBackup.md) so a
#      silent rename breaks the on-call playbook.
#   3. hertznerbox remote name drift — the script (and the ansible
#      postgres role's archive_command) refer to a stable remote
#      name; renaming breaks both at once.
#
# Runs as part of `make lint-pg-restore-verify`. Exit 0 on success.

set -euo pipefail

SCRIPT="$(cd "$(dirname "$0")" && pwd)/pg-restore-verify.sh"

# 1. Syntax check on the verify script. Does NOT execute.
bash -n "$SCRIPT" || { echo "FAIL: bash -n $SCRIPT"; exit 1; }
echo "OK: bash -n"

# 2. Required table names + env-var names + threshold knob.
for tok in "accounts" "apps" "instances" \
           "ROW_COUNT_THRESHOLD" "T_DAYS_BACK" \
           "RESTORE_TEST_ROOT" "RESTORE_PG_PORT" \
           "HETZNER_STORAGE_BOX_BASEBACKUP_PATH" "HETZNER_STORAGE_BOX_WAL_PATH" \
           "rclone" "hertznerbox" "pg_is_in_recovery"; do
  grep -q "$tok" "$SCRIPT" || { echo "FAIL: missing token '$tok' in $SCRIPT"; exit 1; }
done
echo "OK: required tokens present in verify script"

# 3. Step headers present (so a refactor that drops a phase surfaces).
for hdr in "0/5 Pre-flight" "1/5 rclone lsd" "2/5 rclone copy" \
           "3/5 initdb" "4/5 start PG" "5/5 row-count assertions"; do
  grep -q "$hdr" "$SCRIPT" || { echo "FAIL: missing step header '$hdr'"; exit 1; }
done
echo "OK: step headers present"

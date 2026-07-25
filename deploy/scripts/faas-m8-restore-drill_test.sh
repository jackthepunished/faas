#!/usr/bin/env bash
# Smoke unit for the M8 restore drill script. Asserts syntax + token presence
# in the operator-facing template. Catches two regression classes:
#
#   1. Syntax breakage in deploy/scripts/faas-m8-restore-drill.sh — `bash -n`
#      without executing the script. Catches missing closes, typo'd `[[`/`]]`,
#      and unterminated heredocs.
#   2. Drift in the seven required record fields of the template. The bash
#      heredoc in the script's step 7 (added in the next commit) emits each
#      label literally; a refactor that drops one silently breaks the §14
#      M8 audit trail. The same labels are locked by pkg/drills/record_test.go
#      via the embedded template.
#
# Runs as part of `make lint-drill`. Exit 0 on success.

set -euo pipefail

SCRIPT="$(cd "$(dirname "$0")" && pwd)/faas-m8-restore-drill.sh"
TEMPLATE="$(cd "$(dirname "$0")" && pwd)/../../docs/drills/TEMPLATE-restore-drill.md"

# 1. Syntax check on the drill script. Does NOT execute.
bash -n "$SCRIPT" || { echo "FAIL: bash -n $SCRIPT"; exit 1; }
echo "OK: bash -n"

# 2. Required record tokens present in the template the Go test embeds.
for tok in "Wall-clock total" "RPO via basebackup" "RPO via WAL" \
           "Wake latency" "Basebackup SHA-256" \
           "host.age SHA-256" "Verdict"; do
  grep -q "$tok" "$TEMPLATE" || { echo "FAIL: missing token '$tok' in $TEMPLATE"; exit 1; }
done
echo "OK: required tokens present in template"

# 3. Required record tokens present in the script body (step 7 heredoc).
#    Catches drift in the bash heredoc that the Go test cannot see.
for tok in "Wall-clock total" "RPO via basebackup" "RPO via WAL" \
           "Wake latency" "Basebackup SHA-256" \
           "host.age SHA-256" "Verdict"; do
  grep -q "$tok" "$SCRIPT" || { echo "FAIL: missing token '$tok' in $SCRIPT"; exit 1; }
done
echo "OK: required tokens present in drill script"

# 4. Step 0.5 + step 5.5 host.age preservation markers present.
grep -q "0.5/7 Stamp host.age into basebackup" "$SCRIPT" \
  || { echo "FAIL: missing step 0.5 header"; exit 1; }
grep -q "5.5/7 Restore host.age into /etc/faas/secrets" "$SCRIPT" \
  || { echo "FAIL: missing step 5.5 header"; exit 1; }
grep -q "host.age.sha256" "$SCRIPT" \
  || { echo "FAIL: missing host.age SHA sidecar logic"; exit 1; }
echo "OK: host.age preservation steps present"

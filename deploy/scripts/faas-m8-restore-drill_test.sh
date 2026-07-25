#!/usr/bin/env bash
# Smoke unit for the M8 restore drill script. Asserts syntax + token presence.
#
# This is a developer-facing smoke test that runs on macOS or Linux without
# Postgres or systemd. It catches two regression classes:
#
#   1. Syntax breakage in deploy/scripts/faas-m8-restore-drill.sh — `bash -n`
#      without executing the script. Catches missing closes, typo'd `[[`/`]]`,
#      and unterminated heredocs.
#   2. Drift in the seven required record fields. The bash heredoc at the
#      script's step 7 emits each label literally; a refactor that drops one
#      silently breaks the §14 M8 audit trail. The same labels are locked
#      by pkg/drills/record_test.go via the embedded template; this file
#      covers the same contract from the bash side so a refactor that
#      touches only the script is also caught.
#
# Runs as part of `make lint-drill`. Exit 0 on success.

set -euo pipefail

SCRIPT="$(cd "$(dirname "$0")" && pwd)/faas-m8-restore-drill.sh"
TEMPLATE="$(cd "$(dirname "$0")" && pwd)/../../docs/drills/TEMPLATE-restore-drill.md"

# 1. Syntax check on the drill script. Does NOT execute.
bash -n "$SCRIPT" || { echo "FAIL: bash -n $SCRIPT"; exit 1; }
echo "OK: bash -n"

# 2. Required record tokens present in the script body.
for tok in "Wall-clock total" "RPO via basebackup" "RPO via WAL" \
           "Wake latency" "Basebackup SHA-256" \
           "host.age SHA-256" "Verdict"; do
  grep -q "$tok" "$SCRIPT" || { echo "FAIL: missing token '$tok' in $SCRIPT"; exit 1; }
done
echo "OK: required tokens present in drill script"

# 3. Required record tokens present in the template the Go test embeds.
for tok in "Wall-clock total" "RPO via basebackup" "RPO via WAL" \
           "Wake latency" "Basebackup SHA-256" \
           "host.age SHA-256" "Verdict"; do
  grep -q "$tok" "$TEMPLATE" || { echo "FAIL: missing token '$tok' in $TEMPLATE"; exit 1; }
done
echo "OK: required tokens present in template"

#!/bin/bash
sleep 30
for i in 1 2 3 4 5 6 7 8 9 10; do
  STATUS=$(gh run list --workflow ci --branch feat-881-phase-3-per-consumer --limit 1 --json status,conclusion,databaseId 2>&1 | python3 -c "
import json, sys
d = json.load(sys.stdin)
if d:
    print(f'{d[0][\"status\"]} {d[0][\"conclusion\"]} {d[0][\"databaseId\"]}')
else:
    print('empty')
")
  echo "iter=$i status=$STATUS"
  if echo "$STATUS" | grep -q "^completed"; then
    break
  fi
  sleep 30
done
echo "=== FINAL ==="
gh pr view 909 --json statusCheckRollup 2>&1 | python3 -c "
import json, sys
d = json.load(sys.stdin)
for c in d['statusCheckRollup']:
    print(f'{c[\"name\"]}: {c.get(\"conclusion\") or \"pending\"}')
"

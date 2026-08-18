#!/bin/bash
for i in 1 2 3 4 5 6 7 8 9 10; do
  sleep 45
  gh pr view 909 --json statusCheckRollup,mergeable,mergeStateStatus 2>&1 | python3 -c "
import json, sys
d = json.load(sys.stdin)
total = len(d['statusCheckRollup'])
done = sum(1 for c in d['statusCheckRollup'] if c.get('conclusion') in ('SUCCESS','SKIPPED','NEUTRAL','FAILURE'))
failed = [c['name']+':'+c['conclusion'] for c in d['statusCheckRollup'] if c.get('conclusion')=='FAILURE']
pending = [c['name'] for c in d['statusCheckRollup'] if not c.get('conclusion')]
print(f'iter=$i done={done}/{total} fail={failed} pending={pending} mergeable={d[\"mergeable\"]} state={d[\"mergeStateStatus\"]}')
"
  if gh pr view 909 --json statusCheckRollup 2>&1 | python3 -c "
import json, sys
d = json.load(sys.stdin)
total = len(d['statusCheckRollup'])
done = sum(1 for c in d['statusCheckRollup'] if c.get('conclusion') in ('SUCCESS','SKIPPED','NEUTRAL','FAILURE'))
sys.exit(0 if done == total else 1)
"; then
    echo "ALL COMPLETE"
    break
  fi
done
echo "=== FINAL ==="
gh pr view 909 --json statusCheckRollup,mergeable,mergeStateStatus 2>&1 | python3 -c "
import json, sys
d = json.load(sys.stdin)
for c in d['statusCheckRollup']:
    print(f'{c[\"name\"]}: {c.get(\"conclusion\") or \"pending\"}')
print(f'mergeable={d[\"mergeable\"]} state={d[\"mergeStateStatus\"]}')
"

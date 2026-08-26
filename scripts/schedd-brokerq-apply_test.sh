#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
helper="$repo_root/scripts/schedd-brokerq-apply"
tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/schedd-brokerq-test.XXXXXX")
trap 'rm -rf "$tmp_dir"' EXIT

mkdir -p "$tmp_dir/bin"
printf '#!/usr/bin/env bash\nexit 0\n' > "$tmp_dir/bin/ip"
printf '#!/usr/bin/env bash\nprintf "%%s\\n" "$*" > "$BROKERQ_TC_ARGS"\n' > "$tmp_dir/bin/tc"
chmod 0755 "$tmp_dir/bin/ip" "$tmp_dir/bin/tc"

PATH="$tmp_dir/bin:$PATH" BROKERQ_TC_ARGS="$tmp_dir/tc.args" \
  env -u FAAS_BROKER_EGRESS_MBIT "$helper"
test ! -e "$tmp_dir/tc.args"

PATH="$tmp_dir/bin:$PATH" BROKERQ_TC_ARGS="$tmp_dir/tc.args" \
  FAAS_BROKER_EGRESS_MBIT=200 FAAS_BROKER_EGRESS_IFNAME=faas-brokerq \
  "$helper"
test "$(cat "$tmp_dir/tc.args")" = "qdisc replace dev faas-brokerq root tbf rate 200mbit burst 32kbit latency 400ms"

if PATH="$tmp_dir/bin:$PATH" FAAS_BROKER_EGRESS_MBIT=0 "$helper"; then
  echo "expected zero cap to fail" >&2
  exit 1
fi

echo "schedd-brokerq-apply tests passed"

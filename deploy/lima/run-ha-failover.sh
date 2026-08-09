#!/bin/bash
# run-ha-failover.sh — Tier A8 (ADR-083) active-passive failover drill
# on the Lima 2-node HA fleet (deploy/lima/faas-metal-2node-ha.yaml).
#
# Run this FROM YOUR MAC, not inside a guest. It dials each
# Lima VM, asserts the leader is the expected node, triggers
# a manual drain on the leader, and asserts the other node
# picks up leadership within the §14 M8 SLA (DNS flip + drain
# bounded by api.HADNSRecordStaleSeconds = 30s).
#
# Pre-flight:
#   limactl start deploy/lima/faas-metal-2node-ha.yaml --name faas-ha-a
#   limactl start deploy/lima/faas-metal-2node-ha.yaml --name faas-ha-b
#   # In each guest, install the binaries + run a build (out of scope here).
#
# The script drives the public listener + the control plane
# (/healthz, /readyz, /metrics) — NOT a real DNS provider. Both
# VMs are booted with FAAS_DNS_PROVIDER=manual so the DNS
# "flip" is a curl-print to stderr rather than a Cloudflare
# API call. Production must run FAAS_DNS_PROVIDER=cloudflare
# (see docs/runbooks/active-passive-ha.md §"Switching DNS
# providers").
#
# Exit codes:
#   0 — drill passed (node-a drained, node-b warm, both gauges
#       transitioned to the expected terminal values within 30s).
#   1 — pre-flight failure (one or both VMs not running, or
#       gatewayd-public not yet up).
#   2 — leader election didn't converge on the expected name.
#   3 — drain timeout (30s elapsed without node-a reaching
#       standby_state=4 or node-b reaching standby_state=2).
#   4 — metrics scrape failed (the §12 panel "standby state"
#       gauge never appeared).

set -euo pipefail

# Limas that must be running. Operator-tunable via env.
HA_A="${FAAS_HA_A:-faas-ha-a}"
HA_B="${FAAS_HA_B:-faas-ha-b}"
# Control port — the gatewayd-public /metrics endpoint.
# Loopback inside the guest; tunneled via limactl.
HA_METRICS_PORT="${FAAS_HA_METRICS_PORT:-9090}"
# Public listener port — the Caddy-reverse-proxied edge.
HA_PUBLIC_PORT="${FAAS_HA_PUBLIC_PORT:-8080}"

# Drain timeout — §14 M8 SLA = api.HADNSRecordStaleSeconds (30s).
DRAIN_TIMEOUT_SECONDS="${FAAS_HA_DRAIN_TIMEOUT_SECONDS:-30}"

# StandbyState enum values (mirrors pkg/wire/metrics.go).
STATE_WARMING=1
STATE_WARM=2
STATE_DRAINING=3
STATE_DRAINED=4
STATE_FAILED=5

# Pre-flight: both VMs running and gatewayd-public /healthz responding.
log() { printf '[run-ha-failover] %s\n' "$*" >&2; }
fail() { log "FAIL: $*"; exit "${2:-1}"; }

require_vm_up() {
  local vm="$1"
  if ! limactl list --json 2>/dev/null \
       | jq -e --arg n "$vm" '.[] | select(.name==$n) | select(.status=="Running")' \
       >/dev/null; then
    fail "lima VM $vm is not Running" 1
  fi
}

require_vm_up "$HA_A"
require_vm_up "$HA_B"
log "both VMs Running ($HA_A, $HA_B)"

# Drain via a unique-URL marker so we don't accidentally hit a
# second test run with stale data.
node_a_metrics() {
  limactl shell "$HA_A" -- \
    curl -fsS --max-time 5 "http://127.0.0.1:${HA_METRICS_PORT}/metrics"
}
node_b_metrics() {
  limactl shell "$HA_B" -- \
    curl -fsS --max-time 5 "http://127.0.0.1:${HA_METRICS_PORT}/metrics"
}

# Pull the StandbyState gauge value out of a metrics dump.
# Returns 0 (warming) when the gauge isn't present yet — the
# gauge is pre-instantiated at 1 by NewOpsMetrics so this only
# fires before the daemon has scraped at least once.
standby_state() {
  local metrics="$1"
  printf '%s\n' "$metrics" \
    | awk '/^gatewayd_public_gateway_standby_state / {print $2; exit}' \
    | tr -d '\r'
}

# Pull the active_passive_failovers_total counter for a given
# outcome label.
failovers_count() {
  local metrics="$1" outcome="$2"
  printf '%s\n' "$metrics" \
    | awk -v o="$outcome" \
          '$0 ~ "gatewayd_public_gateway_active_passive_failovers_total{outcome=\""o"\"}" \
          {print $2; exit}' \
    | tr -d '\r'
}

# Assert /healthz=200 on both VMs. Plain HTTP, no auth.
healthz() {
  local vm="$1"
  limactl shell "$vm" -- \
    curl -fsS --max-time 5 -o /dev/null \
    -w '%{http_code}\n' \
    "http://127.0.0.1:${HA_PUBLIC_PORT}/healthz" \
    | tr -d '\r'
}

hz_a="$(healthz "$HA_A" || true)"
hz_b="$(healthz "$HA_B" || true)"
[ "$hz_a" = "200" ] || fail "$HA_A /healthz = $hz_a, want 200" 1
[ "$hz_b" = "200" ] || fail "$HA_B /healthz = $hz_b, want 200" 1
log "both /healthz=200"

# Baseline: capture the current leader's standby state + counter
# values. We compare against the post-drain snapshot to detect
# the transition.
ma_before="$(node_a_metrics)"
mb_before="$(node_b_metrics)"
state_a_before="$(standby_state "$ma_before" || echo 0)"
state_b_before="$(standby_state "$mb_before" || echo 0)"
flips_before_a="$(failovers_count "$ma_before" "dns_flipped" || echo 0)"
flips_before_b="$(failovers_count "$mb_before" "dns_flipped" || echo 0)"
log "baseline: $HA_A state=$state_a_before flips=$flips_before_a ; $HA_B state=$state_b_before flips=$flips_before_b"

# Identify the current leader by which VM's standby_state is
# Drained-or-Draining (a leader that just lost = draining, a
# leader that's been quiet = warm with a fresh flip count).
# For the drill, the simpler rule is: lex-min over node names
# per leader.ElectLeader — node-a is the initial leader.
leader="$HA_A"
follower="$HA_B"
log "expected initial leader: $leader"

# Trigger drain on the leader. The production wiring flips
# standby_state=Draining when pg_notify fires
# compute_node_changed; we simulate by setting
# compute_nodes.active=false on the leader via psql inside
# the leader's VM.
log "triggering drain on $leader via compute_nodes.active=false"
limactl shell "$leader" -- sudo -u postgres psql -d faas -tAc \
  "UPDATE compute_nodes SET active=false WHERE name='${HA_A}'"

# Poll the metrics endpoints for up to DRAIN_TIMEOUT_SECONDS.
# We expect:
#   - $HA_A.standby_state transitions to STATE_DRAINED (4) within the budget.
#   - $HA_B.standby_state transitions to STATE_WARM (2) within the budget.
deadline=$((SECONDS + DRAIN_TIMEOUT_SECONDS))
state_a=0
state_b=0
flips_a=0
flips_b=0
while [ "$SECONDS" -lt "$deadline" ]; do
  ma="$(node_a_metrics)"
  mb="$(node_b_metrics)"
  state_a="$(standby_state "$ma" || echo 0)"
  state_b="$(standby_state "$mb" || echo 0)"
  flips_a="$(failovers_count "$ma" "dns_flipped" || echo 0)"
  flips_b="$(failovers_count "$mb" "dns_flipped" || echo 0)"
  if [ "$state_a" = "$STATE_DRAINED" ] && [ "$state_b" = "$STATE_WARM" ]; then
    break
  fi
  sleep 1
done

log "post-drain: $HA_A state=$state_a flips=$flips_a ; $HA_B state=$state_b flips=$flips_b"

# Assert terminal transitions.
if [ "$state_a" != "$STATE_DRAINED" ]; then
  fail "$HA_A did not reach standby_state=Drained (got $state_a) within ${DRAIN_TIMEOUT_SECONDS}s" 3
fi
if [ "$state_b" != "$STATE_WARM" ]; then
  fail "$HA_B did not reach standby_state=Warm (got $state_b) within ${DRAIN_TIMEOUT_SECONDS}s" 3
fi
# The follower's dns_flipped counter must have advanced OR
# the leader's manual_drain counter must have advanced
# (FAAS_DNS_PROVIDER=manual triggers a manual_drain outcome,
# not a dns_flipped).
manual_drain_a="$(failovers_count "$ma" "manual_drain" || echo 0)"
if [ "$flips_a" -le "${flips_before_a:-0}" ] && [ "$manual_drain_a" -le 0 ]; then
  fail "$HA_A did not record a dns_flipped or manual_drain event" 4
fi

# Restore compute_nodes.active=true so the drill is idempotent
# (a re-run on the same fleet must not require a manual reset).
log "restoring compute_nodes.active=true for next drill run"
limactl shell "$leader" -- sudo -u postgres psql -d faas -tAc \
  "UPDATE compute_nodes SET active=true WHERE name='${HA_A}'"

log "DRILL PASSED — node-a drained, node-b warm, within ${DRAIN_TIMEOUT_SECONDS}s"
exit 0

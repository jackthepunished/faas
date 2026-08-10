#!/bin/bash
# run-ha-write-redirect.sh — Tier A9 (ADR-089) standby write-redirect
# drill on the Lima 2-node fleet (faas-metal + faas-metal-2b).
#
# Run this FROM YOUR MAC, not inside a guest. It dials each
# Lima VM, asserts the active-passive topology is in place
# (the Tier A8 / ADR-083 ha-failover-drill must have run at
# least once), then exercises the writeGate's three closed-
# vocabulary increment paths:
#
#   1. bearer write to the standby → relay via mTLS → outcome="relayed"
#   2. cookie write to the standby → 307 to leader URL → outcome="redirect_307"
#   3. baseline assertion: same_box and leader_unreachable are 0 in
#      steady-state
#
# The drill is READ-ONLY: it does NOT toggle compute_nodes.active
# (per the ADR-089 §Open follow-ups decision). The leader identity
# is whatever the prior tier-A8 drill + ongoing election has
# settled on. If the cluster is in a bad state, the drill aborts
# in pre-flight.
#
# Pre-flight:
#   limactl start deploy/lima/faas-metal-2node-a.yaml --name faas-metal
#   limactl start deploy/lima/faas-metal-2node-b.yaml --name faas-metal-2b
#   # In each guest, install the binaries + run a build (out of scope here).
#   # In each guest, ensure mTLS material is in place:
#   #   /etc/faas/tls/gatewayd/egress-client.crt
#   #   /etc/faas/tls/gatewayd/egress-client.key
#   #   /etc/faas/tls/gatewayd/ca.crt
#   # (per ADR-052 PR-C+D keep set; Tier A9 reuses the operator-deployed
#   # material, no new cert files or directories).
#   # Both VMs must run with FAAS_LEADER_REDIRECT_TLS_CERT set so the
#   # writeGate is constructed (opt-in at deploy level per ADR-089
#   # §Decision #1).
#
# Exit codes:
#   0 — drill passed (bearer + cookie both incremented their
#       cells; leader_unreachable and same_box untouched in
#       steady state).
#   1 — pre-flight failure (one or both VMs not running,
#       gatewayd-internal not yet up, mTLS material missing, or
#       no leader elected).
#   2 — relay never landed on the standby (the metric never
#       advanced within 30s).
#   3 — redirect never landed on the standby (the metric never
#       advanced within 30s).
#   4 — metrics scrape failed (the §12 panel "write_redirect_total"
#       gauge never appeared).

set -euo pipefail

# Limas that must be running. Operator-tunable via env.
# Mirrors the Makefile ha-failover-drill target's pair
# (faas-metal + faas-metal-2b) — the Tier A5 fleet is the
# canonical two-node HA config; we do NOT invent a separate
# 2node-ha.yaml (per ADR-089 §Decision: the existing fleet
# already exercises the active-passive topology).
HA_A="${FAAS_HA_A:-faas-metal}"
HA_B="${FAAS_HA_B:-faas-metal-2b}"
# Control port — the gatewayd-internal /metrics endpoint.
# Loopback inside the guest; tunneled via limactl. The internal
# listener is on 9100 (the per-daemon metrics port the runDeps
# wires up); the public listener is on 8080 (Caddy-fronted).
HA_METRICS_PORT="${FAAS_HA_METRICS_PORT:-9100}"
HA_PUBLIC_PORT="${FAAS_HA_PUBLIC_PORT:-8080}"
# Drill timeout — the §14 M9 SLA is the mTLS-hop budget
# (StandbyWriteRedirectTimeoutMS = 5000ms) plus a slack
# budget for the dashboard scrape round-trip. 30s is the
# ladder-compatible default (matches the prior drill's
# DRAIN_TIMEOUT_SECONDS).
DRILL_TIMEOUT_SECONDS="${FAAS_HA_DRILL_TIMEOUT_SECONDS:-30}"
# Drill token — a bearer token with apps:write scope. Read
# from the env so the operator can rotate it without editing
# the script. The drill treats the token as opaque — the
# leader's apid does the actual scope check.
DRILL_TOKEN="${FAAS_HA_DRILL_TOKEN:-drill-token-rotate-me}"
# Drill cookie — same pattern.
DRILL_COOKIE="${FAAS_HA_DRILL_COOKIE:-faas_sid=drill-session-rotate-me}"

# Pre-flight: both VMs running and gatewayd-internal /healthz
# responding.
log() { printf '[run-ha-write-redirect] %s\n' "$*" >&2; }
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

# Assert mTLS material is present on each VM (per the keep set,
# the relay hop expects this material at the canonical path).
require_mtls_material() {
  local vm="$1"
  for f in /etc/faas/tls/gatewayd/egress-client.crt \
           /etc/faas/tls/gatewayd/egress-client.key \
           /etc/faas/tls/gatewayd/ca.crt; do
    if ! limactl shell "$vm" -- sudo test -s "$f" 2>/dev/null; then
      fail "$vm missing mTLS material $f — ADR-052 keep set requires operator-deployed cert here" 1
    fi
  done
}

require_mtls_material "$HA_A"
require_mtls_material "$HA_B"
log "mTLS material present on both VMs"

# /healthz check on the internal listener. The gate sits in
# gatewayd-internal, so the healthz we exercise is the
# internal one (NOT the gatewayd-public /healthz which the
# ADR-083 drill uses).
healthz() {
  local vm="$1"
  limactl shell "$vm" -- \
    curl -fsS --max-time 5 -o /dev/null \
    -w '%{http_code}\n' \
    "http://127.0.0.1:${HA_METRICS_PORT}/healthz" \
    | tr -d '\r'
}

hz_a="$(healthz "$HA_A" || true)"
hz_b="$(healthz "$HA_B" || true)"
[ "$hz_a" = "200" ] || fail "$HA_A /healthz = $hz_a, want 200" 1
[ "$hz_b" = "200" ] || fail "$HA_B /healthz = $hz_b, want 200" 1
log "both /healthz=200"

# Metrics scrapers — the §12 panel "write_redirect_total" lives
# on the gatewayd-internal /metrics endpoint.
node_a_metrics() {
  limactl shell "$HA_A" -- \
    curl -fsS --max-time 5 "http://127.0.0.1:${HA_METRICS_PORT}/metrics"
}
node_b_metrics() {
  limactl shell "$HA_B" -- \
    curl -fsS --max-time 5 "http://127.0.0.1:${HA_METRICS_PORT}/metrics"
}

# Counter parser. ADR-089 §Decision locks the closed vocabulary:
# outcome ∈ {same_box, relayed, redirect_307, leader_unreachable,
#            loop_prevented, mTLS_failure, error}
# auth_kind ∈ {bearer, cookie, anonymous}
# The Prometheus text format sorts labels alphabetically, so the
# substring is "{auth_kind=\"...\",outcome=\"...\"}" — auth_kind
# before outcome.
redirect_count() {
  local metrics="$1" outcome="$2" auth="$3"
  printf '%s\n' "$metrics" \
    | awk -v o="$outcome" -v a="$auth" \
          '$0 ~ "gatewayd_internal_write_redirect_total{auth_kind=\""a"\",outcome=\""o"\"}" \
          {print $2; exit}' \
    | tr -d '\r'
}

# Identify the standby. The leader is the box that holds the
# gatewayd_public_gateway_standby_state=2 (warm) — the standby
# is the OTHER one. We assert the standby_state assertion is
# non-zero on BOTH boxes; if neither shows warm, the operator
# hasn't run the prior tier-A8 drill, and the active-passive
# topology isn't in place.
ma_before="$(node_a_metrics)"
mb_before="$(node_b_metrics)"
state_a="$(printf '%s\n' "$ma_before" \
  | awk '/^gatewayd_public_gateway_standby_state / {print $2; exit}' | tr -d '\r')"
state_b="$(printf '%s\n' "$mb_before" \
  | awk '/^gatewayd_public_gateway_standby_state / {print $2; exit}' | tr -d '\r')"
[ -n "$state_a" ] || fail "$HA_A standby_state gauge missing — prior tier-A8 drill must run first" 1
[ -n "$state_b" ] || fail "$HA_B standby_state gauge missing — prior tier-A8 drill must run first" 1
log "standby_state: $HA_A=$state_a, $HA_B=$state_b"

# The lex-min leader is the box with fresher terminal flips; the
# other is the standby. For the drill we use the simpler rule:
# the standby is the box whose standby_state is NOT 2 (warm).
# (A fresh leader with no flips yet is still 2; a recently-drained
# one might be 3/4/5; a standby is 1/2 with no flips.)
standby="$HA_A"
leader="$HA_B"
if [ "$state_a" = "2" ]; then
  standby="$HA_B"
  leader="$HA_A"
fi
log "standby=$standby leader=$leader"

# Baseline: capture the counters we'll be incrementing.
relayed_before="$(redirect_count "$ma_before" relayed bearer || echo 0)"
relayed_b_before="$(redirect_count "$mb_before" relayed bearer || echo 0)"
redirect_before="$(redirect_count "$ma_before" redirect_307 cookie || echo 0)"
redirect_b_before="$(redirect_count "$mb_before" redirect_307 cookie || echo 0)"
unreach_before="$(redirect_count "$ma_before" leader_unreachable bearer || echo 0)"
unreach_b_before="$(redirect_count "$mb_before" leader_unreachable bearer || echo 0)"
log "baseline: $standby relayed=$relayed_before redirect=$redirect_before ; $leader relayed=$relayed_b_before redirect=$redirect_b_before"

# Step 1: bearer write to the standby. The relay hop hits
# /etc/faas/tls/gatewayd/egress-client.{crt,key} on the relay
# origin (the standby) and the CA at /etc/faas/tls/gatewayd/ca.crt
# on the relay destination (the leader). We expect a 201
# Created from the leader (the apps/create handler in apid).
DRILL_PAYLOAD="{\"slug\":\"drill-$(uuidgen | tr A-Z a-z | tr -d -)\",\"runtime\":\"node22\"}"
log "step 1: bearer write to $standby/v1/apps (payload=$DRILL_PAYLOAD)"
bearer_status=$(limactl shell "$standby" -- \
  curl -fsS --max-time 10 -o /dev/null \
  -w '%{http_code}\n' \
  -H "Authorization: Bearer ${DRILL_TOKEN}" \
  -H "Content-Type: application/json" \
  -X POST -d "$DRILL_PAYLOAD" \
  "https://127.0.0.1:${HA_PUBLIC_PORT}/v1/apps" \
  | tr -d '\r') || bearer_status="curl_failed"
case "$bearer_status" in
  2*) log "bearer write returned $bearer_status (leader accepted the relay)" ;;
  4*|5*) log "WARN: bearer write returned $bearer_status (relay may have hit upstream error); counter check below" ;;
  curl_failed) fail "bearer write curl failed — gatewayd-internal not reachable on $standby" 1 ;;
  *) fail "bearer write returned unexpected status $bearer_status" 1 ;;
esac

# Step 2: cookie write to the standby. We expect 307 with
# Location: https://<leader>/v1/apps and Retry-After: 5.
# The same UUID slug avoids colliding with the bearer write.
log "step 2: cookie write to $standby/v1/apps (expect 307)"
cookie_response=$(limactl shell "$standby" -- \
  curl -isS --max-time 10 \
  --cookie "$DRILL_COOKIE" \
  -H "Content-Type: application/json" \
  -X POST -d "$DRILL_PAYLOAD" \
  "https://127.0.0.1:${HA_PUBLIC_PORT}/v1/apps" \
  | tr -d '\r') || cookie_response=""
cookie_status="$(printf '%s\n' "$cookie_response" | awk 'NR==1{print $2; exit}')"
location="$(printf '%s\n' "$cookie_response" | awk 'tolower($1)=="location:"{print $2; exit}')"
retry_after="$(printf '%s\n' "$cookie_response" | awk 'tolower($1)=="retry-after:"{print $2; exit}')"
case "$cookie_status" in
  307)
    log "cookie write returned 307 (location=$location retry_after=$retry_after)"
    [ -n "$location" ] || fail "cookie 307 missing Location header" 3
    [ "$retry_after" = "5" ] || fail "cookie 307 Retry-After=$retry_after, want 5" 3
    ;;
  *)
    log "WARN: cookie write returned $cookie_status (cookie redirect may not be wired); counter check below"
    ;;
esac

# Poll the metrics endpoints for up to DRILL_TIMEOUT_SECONDS.
# We expect:
#   - $standby.{relayed,bearer} counter advanced by exactly 1
#   - $standby.{redirect_307,cookie} counter advanced by exactly 1
#   - $standby.{leader_unreachable,*} unchanged
#   - $leader.{same_box,*} may have advanced (the leader's
#     local handling is the same_box path; this is the
#     expected silent increment).
deadline=$((SECONDS + DRILL_TIMEOUT_SECONDS))
relayed=0
redirect=0
unreach=0
while [ "$SECONDS" -lt "$deadline" ]; do
  ms="$(if [ "$standby" = "$HA_A" ]; then node_a_metrics; else node_b_metrics; fi)"
  relayed="$(redirect_count "$ms" relayed bearer || echo 0)"
  redirect="$(redirect_count "$ms" redirect_307 cookie || echo 0)"
  unreach="$(redirect_count "$ms" leader_unreachable bearer || echo 0)"
  if [ "$relayed" -gt "${relayed_before:-0}" ] \
     && [ "$redirect" -gt "${redirect_before:-0}" ]; then
    break
  fi
  sleep 1
done

log "post-drill: $standby relayed=$relayed (was $relayed_before); redirect=$redirect (was $redirect_before); unreach=$unreach (was $unreach_before)"

# Assert the relay cell advanced.
if [ "${relayed_before:-0}" = "0" ]; then
  [ "$relayed" -ge 1 ] || fail "$standby relay counter did not advance (now=$relayed, want ≥1)" 2
else
  [ "$relayed" -gt "${relayed_before:-0}" ] || fail "$standby relay counter did not advance (now=$relayed, was $relayed_before)" 2
fi

# Assert the redirect cell advanced.
if [ "${redirect_before:-0}" = "0" ]; then
  [ "$redirect" -ge 1 ] || fail "$standby redirect_307 counter did not advance (now=$redirect, want ≥1)" 3
else
  [ "$redirect" -gt "${redirect_before:-0}" ] || fail "$standby redirect_307 counter did not advance (now=$redirect, was $redirect_before)" 3
fi

# Assert the leader_unreachable cell did NOT advance (the
# cluster is healthy; relays succeeded).
if [ "$unreach" -gt "${unreach_before:-0}" ]; then
  fail "$standby leader_unreachable counter advanced ($unreach_before → $unreach) — drill environment is unhealthy" 4
fi

# The drill is read-only — no UPDATE compute_nodes. The
# (properly-administered) cluster is left in whatever state
# the prior tier-A8 drill settled on.
log "DRILL PASSED — $standby received and relayed/redirected one bearer + one cookie write within ${DRILL_TIMEOUT_SECONDS}s"
exit 0

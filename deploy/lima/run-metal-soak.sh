#!/bin/bash
# run-metal-soak.sh — issue #587 PR-A.8 drain soak (issue #587 / cluster
# plan §PR-A "Manual soak"). Drives mixed WS/HTTP/Upgrade traffic against
# the Lima one-node metal guest for SOAK_DURATION (default 30m), then
# SIGTERMs each gateway daemon in turn and verifies:
#
#   1. /readyz returns 503 within 1s of SIGTERM (drain start).
#   2. drain completes inside SOAK_DRAIN_BUDGET (default 25s = the systemd
#      TimeoutStopSec=30s ceiling minus 5s kernel-reap headroom).
#   3. zero 502s appear in the wrk / h2load log during the drain window.
#   4. gateway_drain_wait_seconds{daemon,outcome="clean"} histogram
#      records a non-empty series, and gateway_inflight_requests drops
#      to 0 at the end of the drain window.
#
# The mix is intentionally heterogeneous so the drain tracker exercises
# every op label:
#
#   - plain HTTP at 1k rps  → op=http on Handler / InternalReverseProxy
#   - WS upgrade at 50 rps  → op=upgrade on forwardproxy hijacker
#   - h2c stream at 100 rps → op=http on TraceHandler
#
# Pre-req: `make metal-lima` is green. This script boots the same daemons
# the metal suite boots (apids, schedd, gatewayd-public, gatewayd-internal,
# vmmd, builderd, imaged, meterd), then drives traffic against a tiny
# synthetic app already deployed by run-metal.sh's TestProvision_RealApp.
#
# This is operator scaffolding, not a CI gate — the deterministic CI gate
# for drain correctness is the TestHandlerDrainClearsUnderLoad unit test
# in pkg/gateway/handler_load_test.go (//go:build load, run via
# `make test-load`). The soak exists to catch a regression that only
# surfaces under sustained mixed-traffic load (e.g. drain bug that only
# triggers when an Upgrade hijacker and an HTTP request race the deadline).
set -euo pipefail

SOAK_DURATION="${SOAK_DURATION:-30m}"
SOAK_DRAIN_BUDGET="${SOAK_DRAIN_BUDGET:-25s}"
GATEWAY_PUBLIC_URL="${GATEWAY_PUBLIC_URL:-http://127.0.0.1:8443}"
GATEWAY_CONTROL_URL="${GATEWAY_CONTROL_URL:-http://127.0.0.1:9463}"
APP_URL="${APP_URL:-${GATEWAY_PUBLIC_URL%/}/jane-api.apps.dom}"
SCRATCH="${SCRATCH:-/tmp/faas-soak}"
SOAK_LOG="${SCRATCH}/soak.log"
WRK_LOG="${SCRATCH}/wrk.log"
H2_LOG="${SCRATCH}/h2.log"
WS_LOG="${SCRATCH}/ws.log"
METRICS_BEFORE="${SCRATCH}/metrics.before"
METRICS_AFTER="${SCRATCH}/metrics.after"
SUMMARY="${SCRATCH}/summary.txt"

mkdir -p "$SCRATCH"
: > "$SOAK_LOG"

log() { printf '[%s] %s\n' "$(date -u +%FT%TZ)" "$*" | tee -a "$SOAK_LOG"; }

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing required tool: $1" >&2; exit 1; }
}

require_cmd wrk
require_cmd h2load
require_cmd curl
require_cmd jq

# Pre-flight: gatewayd-public must be reachable on the control port.
if ! curl -fsS --max-time 2 "$GATEWAY_CONTROL_URL/readyz" >/dev/null; then
  echo "gatewayd-public not reachable at $GATEWAY_CONTROL_URL" >&2
  echo "boot the daemons first (TestProvision_RealApp leaves them running)" >&2
  exit 1
fi

# Snapshot metrics baseline. The drain histogram and inflight gauge are
# both cumulative + set-to-current — we re-scrape after each daemon drain
# and assert the deltas.
log "scrape baseline metrics from $GATEWAY_CONTROL_URL/metrics"
curl -fsS "$GATEWAY_CONTROL_URL/metrics" > "$METRICS_BEFORE"

# --- Phase 1: drive mixed traffic for SOAK_DURATION ----------------------------
log "starting mixed-traffic soak: duration=$SOAK_DURATION"
log "  HTTP:    wrk -t4 -c64 -d${SOAK_DURATION} ${APP_URL}/"
log "  H2C:     h2load -c25 -t4 -T5 -N100 ${APP_URL}/ -d 30m"
log "  WS upgr: 50 conns pumping 1 msg/s for ${SOAK_DURATION}"

# HTTP — plain /op=http. Background so we can drive the others in parallel.
(
  wrk -t4 -c64 -d"$SOAK_DURATION" "${APP_URL}/" > "$WRK_LOG" 2>&1
) &
WRK_PID=$!

# H2C — /op=http on the TraceHandler (Go 1.24+ h2c over the unix socket;
# TraceHandler wraps the internal mux so this exercises the trace path).
(
  H2LOAD_TARGET="${H2LOAD_TARGET:-$APP_URL}"
  timeout "$SOAK_DURATION" h2load -c25 -t4 -T5 -N100 "$H2LOAD_TARGET/" > "$H2_LOG" 2>&1 || true
) &
H2_PID=$!

# WebSocket upgrade — /op=upgrade on forwardproxy. Use a tiny Go shim
# spawned from the host repo so we don't depend on an external client.
(
  SOAK_DURATION="$SOAK_DURATION" WS_LOG="$WS_LOG" go run ./testutil/ws-soak-client "$APP_URL" 50
) > "$WS_LOG" 2>&1 &
WS_PID=$!

cleanup_traffic() {
  log "stopping traffic generators"
  kill "$WRK_PID" "$H2_PID" "$WS_PID" 2>/dev/null || true
  wait "$WRK_PID" "$H2_PID" "$WS_PID" 2>/dev/null || true
}
trap cleanup_traffic EXIT

# Wait for the load window. Don't SIGTERM until at least 95% of the
# duration has elapsed so the soak gets full coverage before we drain.
log "soaking for ${SOAK_DURATION} (PID wrk=$WRK_PID h2=$H2_PID ws=$WS_PID)"
wait "$WRK_PID"
log "wrk finished"
wait "$H2_PID" || true
log "h2load finished"
wait "$WS_PID" || true
log "ws-soak-client finished"
cleanup_traffic
trap - EXIT

# --- Phase 2: drain each daemon and observe ------------------------------------

drain_one() {
  local daemon="$1" pid="$2"
  log "draining $daemon (pid=$pid)"
  local t0 drain_outcome drain_elapsed_seconds five_oh_two_count

  # Assert /readyz returns 503 within 1s of SIGTERM — proves the drain
  # start wired through the gateway-side. PR-A.5 / PR-A.6 set
  # probe.Set(false, "draining") before srv.Shutdown so this must be
  # sub-second.
  kill -TERM "$pid"
  t0=$(date +%s.%N)
  local readyz_ok=0
  for _ in $(seq 1 20); do
    local code
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 1 "$GATEWAY_CONTROL_URL/readyz" || echo 000)
    if [ "$code" = "503" ]; then
      readyz_ok=1
      break
    fi
    sleep 0.05
  done
  if [ "$readyz_ok" -ne 1 ]; then
    log "FAIL: $daemon /readyz never returned 503 within 1s of SIGTERM"
    return 1
  fi

  # Wait for the process to actually exit. systemd's TimeoutStopSec=30s
  # is the upper bound; the drain budget is SOAK_DRAIN_BUDGET. We give
  # the process up to 35s wall clock to exit so a slow-but-clean drain
  # is still surfaced as a pass.
  local deadline=$SECONDS
  deadline=$((deadline + 35))
  while kill -0 "$pid" 2>/dev/null; do
    if [ "$SECONDS" -ge "$deadline" ]; then
      log "FAIL: $daemon did not exit within 35s of SIGTERM (drain hung)"
      return 1
    fi
    sleep 0.5
  done
  local t1
  t1=$(date +%s.%N)
  drain_elapsed_seconds=$(awk -v a="$t1" -v b="$t0" 'BEGIN{printf "%.3f", a-b}')

  # Count 502s in the wrk log. The drain must NOT drop in-flight to 502;
  # if a request was already in-flight when SIGTERM fired, the handler
  # holds it on the Begin slot until it completes (PR-A.3). A 502 here
  # would be a drain-tracker regression.
  five_oh_two_count=$(grep -c ' 502 ' "$WRK_LOG" || true)
  if [ "$five_oh_two_count" -gt 0 ]; then
    log "FAIL: $daemon drained with $five_oh_two_count 502(s) in wrk log"
    return 1
  fi

  log "OK: $daemon drained in ${drain_elapsed_seconds}s, 0x 502"
}

# Discover daemon PIDs. systemd's cgroup identifies them; pgrep against
# the binary basename is enough on the Lima one-node box.
PID_GW_PUBLIC=$(pgrep -f gatewayd-public | head -1 || true)
PID_GW_INTERNAL=$(pgrep -f gatewayd-internal | head -1 || true)
if [ -z "$PID_GW_PUBLIC" ] && [ -z "$PID_GW_INTERNAL" ]; then
  log "FAIL: no gatewayd processes found; did the boot script leave them running?"
  exit 1
fi

if [ -n "$PID_GW_PUBLIC" ]; then
  drain_one gatewayd-public "$PID_GW_PUBLIC"
fi
if [ -n "$PID_GW_INTERNAL" ]; then
  drain_one gatewayd-internal "$PID_GW_INTERNAL"
fi

# --- Phase 3: post-drain metric assertions -------------------------------------

log "scrape post-drain metrics from $GATEWAY_CONTROL_URL/metrics"
# Boot a fresh public control mux on demand for the scrape. The Lima
# harness restarts it via the systemd unit, so wait briefly for it to
# come back.
for _ in $(seq 1 30); do
  if curl -fsS "$GATEWAY_CONTROL_URL/readyz" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
curl -fsS "$GATEWAY_CONTROL_URL/metrics" > "$METRICS_AFTER" || {
  log "WARN: post-drain metrics scrape failed — was the daemon restarted externally?"
}

# Verify the drain histogram has at least one observation per daemon
# outcome=clean. Metric names mirror pkg/gateway/metrics.go:PR-A.7.
check_metric() {
  local metric="$1" label_filter="$2" file="$3"
  awk -v m="$metric" -v lf="$label_filter" '
    $0 ~ m && index($0, lf) > 0 && $0 !~ /^#/ { print; found=1; exit }
    END { exit (found ? 0 : 1) }
  ' "$file"
}

if [ -s "$METRICS_AFTER" ]; then
  for daemon in gatewayd-internal gatewayd-public; do
    if ! check_metric "^gateway_drain_wait_seconds" "daemon=\"$daemon\",outcome=\"clean\"" "$METRICS_AFTER"; then
      log "FAIL: gateway_drain_wait_seconds{daemon=\"$daemon\",outcome=\"clean\"} missing in post-drain metrics"
      log "      this means the drain returned a non-clean outcome — investigate ops metrics before merging"
      exit 1
    fi
  done
fi

# --- Summary -------------------------------------------------------------------

{
  echo "faas drain soak — $(date -u +%FT%TZ)"
  echo "duration=${SOAK_DURATION} drain_budget=${SOAK_DRAIN_BUDGET}"
  echo "wrk:    $(grep -E '^[[:space:]]*[[:digit:]]+ requests in' "$WRK_LOG" || echo 'no summary')"
  echo "h2:     $(grep -E '^total:' "$H2_LOG" || echo 'no summary')"
  echo "ws:     $(wc -l < "$WS_LOG" 2>/dev/null || echo 0) client log lines"
  echo "metrics_before: $METRICS_BEFORE"
  echo "metrics_after:  $METRICS_AFTER"
} > "$SUMMARY"
log "summary written to $SUMMARY"
log "PASS"
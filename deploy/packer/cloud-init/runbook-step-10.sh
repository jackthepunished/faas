#!/usr/bin/env bash
# runbook-step-10.sh — `gregalectl doctor --deep` (gate)
#
# The "node ready" gate (PR #921 / ADR-110 PR-4). Asserts:
#   - every daemon in pkg/daemonunitspec.Registry reports active
#   - every Lifecycle.Probe / ProbeTarget reports ready
#   - on-disk binaries match Release.{firecracker,kernel}_digest
#   - sealed.env + host.age + rclone.conf are present and unsealable
#
# Idempotent: doctor is read-only; always safe to re-run.
#
# Failure semantics: a non-zero exit propagates up through the
# cloud-init runcmd to the operator's metadata API as `node-ready:
# false`. The box stays reachable via SSH but is NOT eligible for
# placement until the doctor passes (per pkg/daemonunitspec
# Probe gates).
set -euo pipefail

source /etc/faas/first-boot.env

gregalectl doctor --deep --output=json > /var/log/faas-first-boot/doctor.json

# Surface a structured success line on stdout so the operator can grep
# serial console / metadata API. The doctor JSON shape is
# {"healthy": bool, "counts": {...}, ...} — the "summary" key does
# NOT exist (PR #929 review-fix M8). jq's // default keeps a sane
# line if the JSON is malformed.
echo "doctor: healthy=$(jq -r '.healthy // false' /var/log/faas-first-boot/doctor.json)"

# Loud structured success — when healthy=true, this surfaces to the
# operator's hcloud metadata API as node-ready: true.
if [ "$(jq -r '.healthy // false' /var/log/faas-first-boot/doctor.json)" = "true" ]; then
    logger -t faas-first-boot "doctor: healthy=true"
else
    logger -t faas-first-boot -p user.err "doctor: healthy=false — node-ready: false"
    exit 1
fi

# Always succeed — doctor itself sets the per-daemon Probe state; this
# script's only job is to surface the result. A non-zero doctor exits
# at the gregalectl call above and the cloud-init runcmd propagates.

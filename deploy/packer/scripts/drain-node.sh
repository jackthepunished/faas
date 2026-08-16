#!/usr/bin/env bash
# deploy/packer/scripts/drain-node.sh — manual drain entry point.
#
# ADR-111 + PR #914: `gregalectl compute-nodes drain --node <fqdn>`
# is the canonical drain command. This shell wrapper is a convenience
# for operators who don't have the gregalectl binary on PATH yet
# (image-build host, recovery shell, etc.).
#
# For the upgrade-node flow, the orchestrator in cmd/deployctl/upgrade.go
# invokes gregalectl directly — this script is for the operator's
# one-off drain scenarios (e.g. "drain fsn-1 before maintenance").
set -euo pipefail

NODE="${1:?node fqdn required}"

# Default to the system-installed gregalectl; fall back to the
# per-release binary if /opt/faas/current exists.
GREGALECTL="${GREGALECTL:-gregalectl}"
if ! command -v "${GREGALECTL}" >/dev/null 2>&1; then
    if [[ -x /opt/faas/current/bin/gregalectl ]]; then
        GREGALECTL=/opt/faas/current/bin/gregalectl
    else
        echo "drain-node: gregalectl not on PATH and /opt/faas/current/bin/gregalectl missing" >&2
        exit 1
    fi
fi

echo "drain-node: draining ${NODE} via ${GREGALECTL}"
exec "${GREGALECTL}" compute-nodes drain --node "${NODE}"

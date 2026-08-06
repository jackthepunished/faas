#!/usr/bin/env bash
# deploy.sh — activate an immutable release on the control-plane host.
# Usage: sudo bash deploy/controlplane/deploy.sh <release-id>

set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 <release-id>" >&2
  exit 2
fi

exec /opt/faas/bin/deployctl deploy "$1"

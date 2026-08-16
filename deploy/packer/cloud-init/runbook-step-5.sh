#!/usr/bin/env bash
# runbook-step-5.sh — `gregalectl host-age init`
#
# Generates the per-box age keypair for X25519 secret-sealing (ADR-020
# / pkg/webhook SealBytes vs SealOne). The host age key is unique per
# box; never shared, never rotated.
#
# Idempotent: if /etc/faas/host.age already exists, exit 0.
set -euo pipefail

if [[ -f /etc/faas/host.age ]]; then
    echo "runbook-step-5: host age keypair already present; skip"
    exit 0
fi

mkdir -p /etc/faas
chmod 0700 /etc/faas

gregalectl host-age init --output=/etc/faas/host.age
chmod 0600 /etc/faas/host.age

echo "runbook-step-5: host age keypair generated"

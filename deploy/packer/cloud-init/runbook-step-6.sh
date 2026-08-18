#!/usr/bin/env bash
# runbook-step-6.sh — `gregalectl sign-keys init`
#
# Generates the per-box cosign keypair for ADR-038 layer attestation
# (every per-app layer is signed by a cosign key the box owns; cross-
# box verification uses the cluster's trust-root).
#
# Idempotent: if /etc/faas/cosign.{key,pub} already exist, exit 0.
set -euo pipefail

if [[ -f /etc/faas/cosign.key && -f /etc/faas/cosign.pub ]]; then
    echo "runbook-step-6: cosign keypair already present; skip"
    exit 0
fi

mkdir -p /etc/faas

gregalectl sign-keys init \
    --private-key=/etc/faas/cosign.key \
    --public-key=/etc/faas/cosign.pub
chmod 0600 /etc/faas/cosign.key
chmod 0644 /etc/faas/cosign.pub

echo "runbook-step-6: cosign keypair generated"

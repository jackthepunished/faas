#!/usr/bin/env bash
# runbook-step-7.sh — `gregalectl node-key init`
#
# Compute-only boxes only. Mints a per-box wire-layer identity used
# for cross-box mTLS (ADR-092): the box's own leaf signed by the
# cluster CA, plus the cluster CA pinned in the box's trust store.
#
# Idempotent: if /etc/faas/node.{crt,key} already exist, exit 0.
# On control-plane boxes, this step is skipped (the box role gate
# already refuses to start cross-box mTLS without a compute-only
# role).
set -euo pipefail

source /etc/faas/first-boot.env

if [[ "${FAAS_BOX_ROLE}" != "compute-only" ]]; then
    echo "runbook-step-7: control-plane box — skipping node-key init"
    exit 0
fi

if [[ -f /etc/faas/node.crt && -f /etc/faas/node.key ]]; then
    echo "runbook-step-7: node keypair already present; skip"
    exit 0
fi

mkdir -p /etc/faas
chmod 0700 /etc/faas

gregalectl node-key init \
    --tls-dir=/etc/faas \
    --cname="$(hostname -f)"

chmod 0600 /etc/faas/node.key
chmod 0644 /etc/faas/node.crt

echo "runbook-step-7: node keypair minted for compute-only"

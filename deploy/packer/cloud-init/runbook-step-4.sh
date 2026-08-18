#!/usr/bin/env bash
# runbook-step-4.sh — `gregalectl pki init --box-role=<role>`
#
# Mints the per-box PKI subset (ADR-092): a per-daemon TLS leaf per box
# signed by the cluster CA. Writes /etc/faas/tls/{apid,schedd,…}.{crt,key}.
#
# Idempotent: if /etc/faas/tls/schedd.{crt,key} already exists, exit 0.
# Re-running on a converged box is a no-op.
set -euo pipefail

source /etc/faas/first-boot.env

if [[ -f /etc/faas/tls/schedd.crt && -f /etc/faas/tls/schedd.key ]]; then
    echo "runbook-step-4: pki subset already present; skip"
    exit 0
fi

mkdir -p /etc/faas/tls
chmod 0700 /etc/faas/tls

# Fetch the cluster CA bundle from the registry. PR #919 lockstep
# guarantees the registry's release_bundles row carries the CA; the
# per-box TLS leaves are minted by gregalectl pki against that CA.
# (No secret material lands in /var/log; per CLAUDE.md "never log
# secret values; env secrets are sealed at rest".)
gregalectl pki init \
    --box-role="${FAAS_BOX_ROLE}" \
    --tls-dir=/etc/faas/tls

echo "runbook-step-4: pki subset minted for ${FAAS_BOX_ROLE}"

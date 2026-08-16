#!/usr/bin/env bash
# runbook-step-9.sh — `gregalectl release install --git-sha X`
#
# Pins /opt/faas/current to the manifest's git_sha (ADR-110). The
# image already bakes /opt/faas/current; release install either:
#   (a) verifies the existing /opt/faas/current matches the manifest's
#       git_sha (no-op, idempotent), or
#   (b) fetches + verifies the release from the registry (initial
#       install on a fresh image).
#
# Idempotent: if /opt/faas/current/VERSION already matches
# FAAS_MANIFEST_GIT_SHA, exit 0.
set -euo pipefail

source /etc/faas/first-boot.env

CURRENT_SHA="$(cat /opt/faas/current/VERSION 2>/dev/null || echo missing)"

if [[ "${CURRENT_SHA}" == "${FAAS_MANIFEST_GIT_SHA}" ]]; then
    echo "runbook-step-9: /opt/faas/current already pinned to ${FAAS_MANIFEST_GIT_SHA}; skip"
    exit 0
fi

# Registry DSN is supplied via /etc/faas/registry.env (written by
# pki init at runbook-step-4). The release install fetches from the
# registry + verifies the manifest's release_bundles row (PR #919
# lockstep contract).
gregalectl release install \
    --git-sha="${FAAS_MANIFEST_GIT_SHA}" \
    --target=/opt/faas/current \
    --verify

# Stamp the VERSION file so the next run is idempotent.
echo "${FAAS_MANIFEST_GIT_SHA}" > /opt/faas/current/VERSION

echo "runbook-step-9: /opt/faas/current pinned to ${FAAS_MANIFEST_GIT_SHA}"

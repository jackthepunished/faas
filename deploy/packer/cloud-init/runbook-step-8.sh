#!/usr/bin/env bash
# runbook-step-8.sh — `gregalectl backup init && unseal-rclone`
#
# Sets up off-host Storage Box creds (issue #250) sealed by the host
# age key. rclone.conf carries the per-box access keys; unseal-rclone
# decrypts them at runtime into an in-memory cache (never written
# back to /etc/faas/rclone.conf unredacted).
#
# Idempotent: if /etc/faas/rclone.conf.age (sealed) already exists
# AND `rclone listremotes` returns the configured remote, exit 0.
set -euo pipefail

source /etc/faas/first-boot.env

if [[ -f /etc/faas/rclone.conf.age ]]; then
    # Re-verify the unseal works (the host age key may have changed).
    if gregalectl backup unseal-rclone --check 2>/dev/null; then
        echo "runbook-step-8: rclone already initialised + unsealable; skip"
        exit 0
    fi
fi

mkdir -p /etc/faas
chmod 0700 /etc/faas

# Fetch the rclone.conf from the off-host registry using the box's
# per-host Storage Box token (supplied via the cloud-init metadata
# blob the operator configures when commissioning the box).
if [[ -z "${RCLONE_STORAGE_BOX_TOKEN:-}" ]]; then
    echo "runbook-step-8: RCLONE_STORAGE_BOX_TOKEN not present in metadata" >&2
    exit 1
fi

gregalectl backup init \
    --storage-box-token="${RCLONE_STORAGE_BOX_TOKEN}" \
    --sealed-output=/etc/faas/rclone.conf.age

# Seal: rclone.conf on disk is the sealed form; the unseal happens
# at runtime into memory only. Per pkg/webhook SealBytes lesson:
# use SealBytes for blobs (not SealOne — that's for env vars).
chmod 0600 /etc/faas/rclone.conf.age

echo "runbook-step-8: rclone initialised + sealed"

#!/usr/bin/env bash
# scripts/verify-no-secrets.sh — fail-closed if any forbidden secret path
# is present in the in-build VM. Called as the LAST provisioner; on
# success, Packer snapshots the VM into the image tag.
#
# Per ADR-111: image MUST NOT contain /etc/faas/sealed.env, host.age,
# rclone.conf, cosign.{key,pub}, or any *.key under /etc/faas/tls/.
# These land at first-boot via `gregalectl {pki,host-age,sign-keys,
# node-key,backup} init` (runbook steps 4-8 in
# docs/runbooks/manifest-renderer-cutover.md).
#
# This is the load-bearing tripwire: if the build pipeline accidentally
# baked secrets into the image, this script fails the build BEFORE the
# snapshot is taken. CI runs the same scan on the published snapshot in
# .github/workflows/ci.yml:image-build-canary.
set -euo pipefail

# Forbidden paths (relative to /). Globs match files OR directories.
FORBIDDEN=(
    "/etc/faas/sealed.env"
    "/etc/faas/host.age"
    "/etc/faas/host.age.pub"
    "/etc/faas/rclone.conf"
    "/etc/faas/cosign.key"
    "/etc/faas/cosign.pub"
    "/etc/faas/tls"  # entire directory; per-daemon *.key would be here
)

FOUND=0
for path in "${FORBIDDEN[@]}"; do
    # -e: file exists; -d: dir exists. Either trip the build.
    if [[ -e "${path}" || -d "${path}" ]]; then
        echo "verify-no-secrets: FORBIDDEN path present: ${path}" >&2
        FOUND=1
    fi
done

# Also scan for any *.key, *.pem, or *age files under /etc/faas (catches
# accidental copies). This is broader than the explicit list above; the
# explicit list is the audit list, this is the safety net.
if find /etc/faas -type f \( -name '*.key' -o -name '*.pem' -o -name '*.age' \) 2>/dev/null | grep -q .; then
    echo "verify-no-secrets: forbidden *.key / *.pem / *.age present under /etc/faas" >&2
    find /etc/faas -type f \( -name '*.key' -o -name '*.pem' -o -name '*.age' \) >&2
    FOUND=1
fi

if [[ "${FOUND}" -ne 0 ]]; then
    echo "verify-no-secrets: FAIL — secrets present in image; image build aborted" >&2
    exit 1
fi

echo "verify-no-secrets: OK (no forbidden paths)"

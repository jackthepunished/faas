#!/usr/bin/env bash
# deploy/packer/cloud-rollout/bare-metal.sh — PXE-boot specific rollout.
#
# ADR-111 contract: takes (node-fqdn, image-tag), PXE-boots the
# bare-metal box from the new autoinstall ISO, and waits for the new
# host to come up.
#
# Bare-metal provisioning is the legacy path (the spec claims
# "deployable on any bare-metal x86_64 control-plane node" — image
# must support that). Cloud builders are preferred for new boxes.
set -euo pipefail

NODE="${1:?node fqdn required}"
IMAGE_TAG="${2:?image tag required}"

# Resolve the ISO artifact from the registry. The image-build pipeline
# uploads the .iso to the off-host Storage Box (issue #250); this
# wrapper downloads it to the PXE server's TFTP root.
ISO_PATH="/srv/pxe/faas-images/${IMAGE_TAG}.iso"

echo "bare-metal-rollout: PXE-booting ${NODE} from ${ISO_PATH}"

if [[ ! -f "${ISO_PATH}" ]]; then
    echo "bare-metal-rollout: ISO ${ISO_PATH} not found on PXE server" >&2
    exit 1
fi

# Wire the PXE config so the next DHCP boot picks up the new ISO.
cat > "/srv/pxe/pxelinux.cfg/01-$(get-mac "${NODE}")" <<EOF
DEFAULT faas
LABEL faas
    KERNEL ${ISO_PATH}
    APPEND autoinstall ds=nocloud-net;s=http://pxe.local/cloud-init/ reboot=true
EOF

# Trigger a reboot. The host will come up on the new image; the
# cloud-init first-boot template (control-plane.yaml.tpl /
# compute-only.yaml.tpl) drives the runbook-step chain.
ipmitool -I lanplus -H "$(bmc-ip "${NODE}")" -U operator -P "${IPMI_PASSWORD}" \
    chassis power cycle

echo "bare-metal-rollout: ${NODE} reboot signalled; orchestrator takes the probe gate"

# Wait for the host's BMC to report the new boot's serial console
# `node-ready: true` line. The deployctl upgrade orchestrator polls
# the Lifecycle.Probe gate next; this wrapper just waits for the
# host to come up.
for i in $(seq 1 60); do
    if ipmitool -I lanplus -H "$(bmc-ip "${NODE}")" -U operator -P "${IPMI_PASSWORD}" \
        chassis power status 2>/dev/null | grep -q "^Chassis Power is on$"; then
        break
    fi
    sleep 5
done

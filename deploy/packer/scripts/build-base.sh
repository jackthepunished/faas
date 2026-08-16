#!/usr/bin/env bash
# scripts/build-base.sh — run the canonical 23 ansible roles against the
# in-build VM. Same shape as `make bootstrap*`; image build freezes the
# output of those roles into the image.
#
# Per ADR-111: image and bootstrap share the same ansible tree. Image
# is the production host; bootstrap is the dev/CI installer. Bootstrap
# is the seed for the first image build.
#
# This script runs INSIDE the Packer VM (NOT on the operator's host).
# The /tmp/src mount contains the cloned repo; we run ansible against
# the localhost.
set -euo pipefail

SRC_ROOT="${SRC_ROOT:-/tmp/src}"
FAAS_BOX_ROLE="${FAAS_BOX_ROLE:-control-plane}"
FAAS_GIT_SHA="${FAAS_GIT_SHA:-gitsha-not-set}"

cd "${SRC_ROOT}"

# ansible + git are pre-installed by the build host's base image; the
# iso.pkr.hcl builder installs them explicitly. Verify:
command -v ansible-playbook >/dev/null 2>&1 || apt-get install -y --no-install-recommends ansible git

# Pin the per-role inventory to the in-build VM. bootstrap.yml expects
# a host inventory; we point at localhost and override faas_box_role.
ansible-playbook -i "localhost," -c local \
    deploy/ansible/bootstrap.yml \
    -e "faas_box_role=${FAAS_BOX_ROLE}" \
    -e "faas_git_sha=${FAAS_GIT_SHA}" \
    --diff

# /etc/profile.d/go.sh so the runtime PATH picks up /usr/local/go/bin.
cat > /etc/profile.d/go.sh <<'EOF'
export PATH="/usr/local/go/bin:/opt/faas/current/bin:${PATH}"
export FAAS_BOX_ROLE
EOF
chmod 0644 /etc/profile.d/go.sh

# Kernel kargs (per CLAUDE.md §11). These belong to boot, not bake — but
# on bare-metal we set them via grub config; on cloud we rely on the
# provider's `cmdline` knob. The cloud-init user-data (PR #930) handles
# the cloud side; this script handles the bare-metal / ISO side.
if [[ -f /etc/default/grub ]]; then
    sed -i 's/^GRUB_CMDLINE_LINUX_DEFAULT="\(.*\)"/GRUB_CMDLINE_LINUX_DEFAULT="\1 cgroup_no_v1=0 unprivileged_userns_clone=0"/' \
        /etc/default/grub
    if command -v update-grub >/dev/null 2>&1; then
        update-grub
    fi
fi

# Per-role 99-faas-role.conf drop-in (Mega-PR-C Commit 4). Iterated
# over the role-overlay.pkr.hcl's local.active_daemons subset.
DAEMONS_BY_ROLE='{"control-plane":["apid","schedd","vmmd","imaged","meterd","builderd","gatewayd-internal","gatewayd-public"],"compute-only":["vmmd","imaged","meterd","gatewayd-internal","gatewayd-public"]}'
DAEMON_LIST=$(echo "${DAEMONS_BY_ROLE}" | python3 -c "import json,sys; print(' '.join(json.load(sys.stdin)['${FAAS_BOX_ROLE}']))")

for d in ${DAEMON_LIST}; do
    DROP_IN_DIR="/etc/systemd/system/faas-${d}.service.d"
    mkdir -p "${DROP_IN_DIR}"
    cat > "${DROP_IN_DIR}/99-faas-role.conf" <<EOF
[Service]
Environment=FAAS_BOX_ROLE=${FAAS_BOX_ROLE}
Environment=FAAS_$(echo "${d}" | tr '[:lower:]' '[:upper:]' | tr '-' '_')_ROLE=${FAAS_BOX_ROLE}
EOF
done

echo "build-base: applied ${FAAS_BOX_ROLE} role to ${DAEMON_LIST}"

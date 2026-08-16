#!/usr/bin/env bash
# scripts/build-base.sh — run the canonical 23 ansible roles against the
# in-build VM. Same shape as `make bootstrap*`; image build freezes the
# output of those roles into the image.
#
# Per ADR-112: image and bootstrap share the same ansible tree. Image
# is the production host; bootstrap is the dev/CI installer. Bootstrap
# is the seed for the first image build.
#
# IMPORTANT — ADR-112 role-image-collapse: the image is role-AGNOSTIC.
# This script does NOT inject a role. Every box boots from the same
# image; per-daemon drop-ins (99-faas-role.conf) are templated at
# first-boot by `gregalectl release install --role`, keyed off
# `FAAS_BOX_ROLE` from the cloud-init user-data. The image bakes ALL
# 8 daemon binaries as on-disk binaries + a stub
# `/etc/faas/role/role.conf.tpl` template that first-boot reads.
#
# This script runs INSIDE the Packer VM (NOT on the operator's host).
# The /tmp/src mount contains the cloned repo; we run ansible against
# the localhost.
set -euo pipefail

SRC_ROOT="${SRC_ROOT:-/tmp/src}"
FAAS_GIT_SHA="${FAAS_GIT_SHA:-gitsha-not-set}"

cd "${SRC_ROOT}"

# ansible + git are pre-installed by the build host's base image; the
# iso.pkr.hcl builder installs them explicitly. Verify:
command -v ansible-playbook >/dev/null 2>&1 || apt-get install -y --no-install-recommends ansible git

# Pin the inventory to the in-build VM. bootstrap.yml expects a host
# inventory; we point at localhost.
#
# ADR-112: ansible runs with no faas_box_role override. The image is
# role-agnostic; first-boot sets FAAS_BOX_ROLE via cloud-init
# user-data and `gregalectl release install --role` does the
# templating.
ansible-playbook -i "localhost," -c local \
    deploy/ansible/bootstrap.yml \
    -e "faas_git_sha=${FAAS_GIT_SHA}" \
    --diff

# /etc/profile.d/go.sh so the runtime PATH picks up /usr/local/go/bin.
# ADR-112: no FAAS_BOX_ROLE export — role is set per-exec by
# `gregalectl release install --role`. Daemons that need the value
# read it from /etc/faas/first-boot.env at startup time.
cat > /etc/profile.d/go.sh <<'EOF'
export PATH="/usr/local/go/bin:/opt/faas/current/bin:${PATH}"
EOF
chmod 0644 /etc/profile.d/go.sh

# Kernel kargs (per CLAUDE.md §11) — handled in the install pass below.
# The block is idempotent (guarded by `grep -q cgroup_no_v1=0`) and adds
# `systemd.unified_cgroup_hierarchy=1` so cgroups v2 is enforced.
#
# PR #929 review-fix M11: this section used to non-idempotently sed
# GRUB_CMDLINE_LINUX_DEFAULT on every run, doubling the args after
# re-builds; the new block at the bottom replaces it.

# Install the first-boot runbook-step scripts + verify-secrets.sh into
# the image at /usr/local/bin/faas-first-boot/. The cloud-init runcmd
# invokes them by absolute path; without this install pass, the
# runbook can't run (PR #929 review-fix M10).
install -d -m 0755 /usr/local/bin/faas-first-boot
for s in "${SRC_ROOT}/deploy/packer/cloud-init/runbook-step-"*.sh; do
    install -m 0755 "${s}" /usr/local/bin/faas-first-boot/
done
# verify-secrets.sh is the canonical schema check (same script the
# in-build verify-no-secrets.sh runs at build time). Lives at
# deploy/scripts/verify-secrets.sh.
if [[ -f "${SRC_ROOT}/deploy/scripts/verify-secrets.sh" ]]; then
    install -m 0755 "${SRC_ROOT}/deploy/scripts/verify-secrets.sh" \
        /usr/local/bin/faas-first-boot/verify-secrets.sh
fi

# ADR-112: drop the role-template stub that first-boot
# `gregalectl release install --role` reads to write per-daemon
# 99-faas-role.conf drop-ins. The template body is intentionally
# generic — the actual role string is supplied at first-boot. The
# template substitution is {{ROLE}} -> control-plane | compute-only.
install -d -m 0755 /etc/faas/role
cat > /etc/faas/role/role.conf.tpl <<'EOF'
[Service]
Environment=FAAS_BOX_ROLE={{ROLE}}
Environment=FAAS_{{DAEMON_UPPER}}_ROLE={{ROLE}}
EOF
chmod 0644 /etc/faas/role/role.conf.tpl

# Drop the role overlay's kernel args into GRUB_CMDLINE_LINUX (NOT
# GRUB_CMDLINE_LINUX_DEFAULT — DEFAULT only shows on the recovery
# menu; LINUX is what normal boots use). Idempotent: skip if the args
# are already present (PR #929 review-fix M11).
if [[ -f /etc/default/grub ]]; then
    if ! grep -q 'cgroup_no_v1=0' /etc/default/grub; then
        sed -i 's/^GRUB_CMDLINE_LINUX="\(.*\)"/GRUB_CMDLINE_LINUX="\1 cgroup_no_v1=0 unprivileged_userns_clone=0 systemd.unified_cgroup_hierarchy=1"/' \
            /etc/default/grub
        # Also drop the args into DEFAULT so recovery menu sees them.
        sed -i 's/^GRUB_CMDLINE_LINUX_DEFAULT="\(.*\)"/GRUB_CMDLINE_LINUX_DEFAULT="\1 cgroup_no_v1=0 unprivileged_userns_clone=0 systemd.unified_cgroup_hierarchy=1"/' \
            /etc/default/grub
        if command -v update-grub >/dev/null 2>&1; then
            update-grub
        fi
    fi
fi

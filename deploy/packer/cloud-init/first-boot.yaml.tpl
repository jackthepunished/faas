#cloud-config
# deploy/packer/cloud-init/first-boot.yaml.tpl — first-boot user-data
# for the Gregale Compute Image (ADR-112).
#
# ADR-112 role-image-collapse:
#   This template is the SINGLE first-boot user-data for every box.
#   Per-role distinction (control-plane vs compute-only) lives in
#   `FAAS_BOX_ROLE`, which the operator passes via cloud-init user-data
#   (NOT a template variable). The image bytes are identical for both
#   roles; `gregalectl release install --role $FAAS_BOX_ROLE` does the
#   templating at first-boot.
#
# Idempotency contract:
#   - Every runbook-step script under /usr/local/bin/faas-first-boot/
#     short-circuits if its target state already exists.
#   - Re-running user-data on a converged box is a no-op (no daemon
#     restarts, no sealed.env re-seals, no host-age regen).
#   - gregalectl release install + gregalectl doctor are safe to re-run
#     (the release install checks the manifest hash; doctor is
#     read-only).
#
# Per ADR-111 + docs/runbooks/manifest-renderer-cutover.md (steps 4-10):
#   1. pki init --box-role=<role>      (step 4)
#   2. host-age init                   (step 5)
#   3. sign-keys init                  (step 6)
#   4. node-key init                   (step 7 — compute-only only;
#                                       no-op on control-plane via
#                                       the per-step script's role guard)
#   5. backup init + unseal-rclone     (step 8)
#   6. release install --git-sha       (step 9)
#   7. release install --role          (step 9b — NEW under ADR-112;
#                                       writes per-daemon drop-ins +
#                                       starts the role-appropriate
#                                       subset)
#   8. doctor --deep                   (step 10 — gate)

bootcmd:
  # Kernel cmdline (CLAUDE.md §11): cgroups v2 only, no unprivileged
  # userns clone. The image already GRUBs these for the iso path; the
  # cloud provider's launch-template is responsible for the cloud path.
  # Here we surface a loud log line so the operator can grep the
  # serial console for it during a rollout.
  - |
    logger -t faas-first-boot \
      "gregale-compute fc{{fc_release}} kernel{{kernel_version}} g{{git_sha}} — boot $(date -u +%FT%TZ)"
  - |
    mkdir -p /var/log/faas-first-boot && \
      echo "started at $(date -u +%FT%TZ)" > /var/log/faas-first-boot/runbook.log

# ADR-112: FAAS_BOX_ROLE is operator-supplied (via override / user-data
# at server-create time), NOT a template variable. The template renders
# the same bytes for every box; the role string is what differs.
write_files:
  - path: /etc/faas/first-boot.env
    permissions: '0600'
    owner: root:root
    content: |
      # Generated from the cloud-init user-data at server-create time
      # by the operator. ADR-112: image + bootstrap separation. The
      # image is role-agnostic; the role string here decides which
      # daemons first-boot starts.
      FAAS_BOX_ROLE=__SET_BY_OPERATOR_AT_LAUNCH__
      FAAS_FC_RELEASE={{fc_release}}
      FAAS_KERNEL_VERSION={{kernel_version}}
      FAAS_GIT_SHA={{git_sha}}
      FAAS_MANIFEST_GIT_SHA={{git_sha}}

  # The 6 runbook-step scripts are installed into /usr/local/bin/faas-
  # first-boot/ by the image (deploy/packer/scripts/build-base.sh
  # copies them from SRC_ROOT/deploy/packer/cloud-init/runbook-step-*.
  # sh). This file is the cloud-init ENTRY; the per-step scripts do
  # the work.
  - path: /usr/local/bin/faas-first-boot/assert-first-boot-env.sh
    permissions: '0755'
    owner: root:root
    content: |
      #!/usr/bin/env bash
      # assert-first-boot-env.sh — fail-closed if the operator did not
      # supply FAAS_BOX_ROLE via cloud-init user-data override. ADR-112
      # makes FAAS_BOX_ROLE load-bearing; without it, first-boot can't
      # decide which daemons to start.
      set -euo pipefail
      if grep -q '^FAAS_BOX_ROLE=__SET_BY_OPERATOR_AT_LAUNCH__$' \
          /etc/faas/first-boot.env; then
        logger -t faas-first-boot -p user.err \
          "FAAS_BOX_ROLE is the sentinel; the operator MUST override it via cloud-init user-data (ADR-112). Set FAAS_BOX_ROLE=control-plane|compute-only in /etc/faas/first-boot.env and re-run."
        exit 11
      fi
      grep -qE '^FAAS_BOX_ROLE=(control-plane|compute-only)$' \
          /etc/faas/first-boot.env \
        || { logger -t faas-first-boot -p user.err \
          "FAAS_BOX_ROLE value is not control-plane|compute-only; refusing to first-boot."; exit 12; }

runcmd:
  # Execute the 7-step init chain. Each script is idempotent; if a
  # step has already completed (file exists, daemon running), it
  # exits 0 with no side effects.
  - |
    set -euxo pipefail
    /usr/local/bin/faas-first-boot/assert-first-boot-env.sh
    source /etc/faas/first-boot.env
    for step in 4 5 6 7 8 9 10; do
      echo "runbook-step-${step} starting at $(date -u +%FT%TZ)" \
        | tee -a /var/log/faas-first-boot/runbook.log
      /usr/local/bin/faas-first-boot/runbook-step-${step}.sh \
        || RC=$?
      if [ "${RC:-0}" -ne 0 ]; then
        echo "runbook-step-${step} FAILED with rc=${RC}" \
          | tee -a /var/log/faas-first-boot/runbook.log
        # Loud, structured failure so the operator can grep serial
        # console + hcloud metadata API surfaces it as 'node-ready: false'.
        logger -t faas-first-boot -p user.err \
          "node-ready: false (runbook-step-${step} rc=${RC})"
        exit "${RC}"
      fi
    done
    logger -t faas-first-boot "node-ready: true"
    echo "runbook complete at $(date -u +%FT%TZ)" \
      | tee -a /var/log/faas-first-boot/runbook.log

# power_state: do NOT shutdown — we want the box ready after init.
power_state:
  mode: reboot
  condition: false

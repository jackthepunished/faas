#cloud-config
# deploy/packer/cloud-init/control-plane.yaml.tpl — first-boot user-data
# for control-plane Gregale Compute Images (ADR-111).
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
# Per ADR-111 + docs/runbooks/manifest-renderer-cutover.md (steps 4-8):
#   1. pki init --box-role=<role>
#   2. host-age init
#   3. sign-keys init
#   4. node-key init  (compute-only only — NOT this template)
#   5. backup init + unseal-rclone
#   6. release install --git-sha $MANIFEST_GIT_SHA
#   7. doctor --deep  (gate; failure halts the user-data)

# Run on every boot; per-step scripts guard themselves.
# (cloud-init's default frequency is 'once-per-instance' but our
# per-step short-circuits make 'always' safe + match the operator's
# "re-run user-data" workflow.)
bootcmd:
  # Kernel cmdline (CLAUDE.md §11): cgroups v2 only, no unprivileged
  # userns clone. The image already GRUBs these for the iso path; the
  # cloud provider's launch-template is responsible for the cloud path.
  # Here we surface a loud log line so the operator can grep the
  # serial console for it during a rollout.
  - |
    logger -t faas-first-boot \
      "control-plane image {{role}} fc{{fc_release}} kernel{{kernel_version}} g{{git_sha}} — boot $(date -u +%FT%TZ)"
  - |
    mkdir -p /var/log/faas-first-boot && \
      echo "started at $(date -u +%FT%TZ)" > /var/log/faas-first-boot/runbook.log

write_files:
  # The 6 runbook-step scripts are installed into /usr/local/bin/faas-
  # first-boot/ by the image (deploy/packer/scripts/build-base.sh
  # copies them from SRC_ROOT/deploy/packer/cloud-init/runbook-step-*.
  # sh). This file is the cloud-init ENTRY; the per-step scripts do
  # the work.
  - path: /etc/faas/first-boot.env
    permissions: '0600'
    owner: root:root
    content: |
      # Generated from the cloud-init template at boot.
      FAAS_BOX_ROLE={{role}}
      FAAS_FC_RELEASE={{fc_release}}
      FAAS_KERNEL_VERSION={{kernel_version}}
      FAAS_GIT_SHA={{git_sha}}
      FAAS_MANIFEST_GIT_SHA={{git_sha}}

runcmd:
  # Execute the 6-step init chain. Each script is idempotent; if a
  # step has already completed (file exists, daemon running), it
  # exits 0 with no side effects.
  - |
    set -euxo pipefail
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

#cloud-config
# deploy/packer/cloud-init/compute-only.yaml.tpl — first-boot user-data
# for compute-only Gregale Compute Images (ADR-111).
#
# Same idempotency contract as control-plane.yaml.tpl. Differs in:
#   - FAAS_BOX_ROLE=compute-only
#   - runbook step 7 (node-key init) is INCLUDED (compute-only boxes
#     need a per-box wire-layer identity for cross-box mTLS, ADR-092)
#   - The per-daemon drop-ins are filtered to the compute-only subset
#     (vmmd, imaged, meterd, gatewayd-internal, gatewayd-public)
#
# Re-running user-data on a converged compute-only box is a no-op.

bootcmd:
  - |
    logger -t faas-first-boot \
      "compute-only image {{role}} fc{{fc_release}} kernel{{kernel_version}} g{{git_sha}} — boot $(date -u +%FT%TZ)"
  - |
    mkdir -p /var/log/faas-first-boot && \
      echo "started at $(date -u +%FT%TZ)" > /var/log/faas-first-boot/runbook.log

write_files:
  - path: /etc/faas/first-boot.env
    permissions: '0600'
    owner: root:root
    content: |
      FAAS_BOX_ROLE={{role}}
      FAAS_FC_RELEASE={{fc_release}}
      FAAS_KERNEL_VERSION={{kernel_version}}
      FAAS_GIT_SHA={{git_sha}}
      FAAS_MANIFEST_GIT_SHA={{git_sha}}

# Compute-only boxes need runbook step 7 (node-key init) for cross-box
# mTLS (ADR-092). The runcmd below iterates steps 4-10 with 7 included;
# the per-step scripts short-circuit if their target already exists.
runcmd:
  - |
    set -euxo pipefail
    source /etc/faas/first-boot.env
    for step in 4 5 6 7 8 9 10; do
      echo "runbook-step-${step} starting at $(date -u +%FT%TZ)" \
        | tee -a /var/log/faas-first-boot/runbook.log
      /usr/local/bin/faas-first-boot/runbook-step-${step}.sh || RC=$?
      if [ "${RC:-0}" -ne 0 ]; then
        echo "runbook-step-${step} FAILED with rc=${RC}" \
          | tee -a /var/log/faas-first-boot/runbook.log
        logger -t faas-first-boot -p user.err \
          "node-ready: false (runbook-step-${step} rc=${RC})"
        exit "${RC}"
      fi
    done
    logger -t faas-first-boot "node-ready: true"
    echo "runbook complete at $(date -u +%FT%TZ)" \
      | tee -a /var/log/faas-first-boot/runbook.log

power_state:
  mode: reboot
  condition: false

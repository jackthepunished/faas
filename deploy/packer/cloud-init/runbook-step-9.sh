#!/usr/bin/env bash
# runbook-step-9.sh — `gregalectl release install --git-sha X --role Y`
#
# ADR-112 role-image-collapse: this step does TWO things now:
#   (1) Pin /opt/faas/current to the manifest's git_sha (ADR-110).
#       gregalectl release install flips /opt/faas/current →
#       /opt/faas/releases/<sha>; the image already bakes a default
#       /opt/faas/current (gitsha-not-set) so the flip is the
#       load-bearing step.
#   (2) Apply the role templating (99-faas-role.conf drop-ins +
#       systemctl daemon-reload + start the role-appropriate subset).
#       This is the NEW second invocation (also `gregalectl
#       release install --role`). The first invocation could
#       short-circuit on idempotency; the second only runs when
#       either the FAAS_BOX_ROLE env is set in first-boot.env or
#       the operator passes --role explicitly.
#
# Per ADR-112:
#   - Same image bytes for every role; FAAS_BOX_ROLE decides which
#     drop-ins get written and which daemons get started.
#   - Re-running this step on a converged box is a no-op: the
#     `release install --git-sha` skips on VERSION match; the
#     `release install --role` is idempotent (drop-ins are
#     byte-for-byte stable).
#
# Idempotent: if /opt/faas/current/VERSION already matches
# FAAS_MANIFEST_GIT_SHA AND the drop-ins for FAAS_BOX_ROLE already
# match the canonical body, exit 0.

set -euo pipefail

source /etc/faas/first-boot.env

CURRENT_SHA="$(cat /opt/faas/current/VERSION 2>/dev/null || echo missing)"

if [[ "${CURRENT_SHA}" == "${FAAS_MANIFEST_GIT_SHA}" ]]; then
    echo "runbook-step-9: /opt/faas/current already pinned to ${FAAS_MANIFEST_GIT_SHA}; still running --role for safety"
else
    # Registry DSN is supplied via /etc/faas/registry.env (written
    # by pki init at runbook-step-4). The release install fetches
    # from the registry + verifies the manifest's release_bundles
    # row (PR #919 lockstep contract).
    gregalectl release install \
        --git-sha="${FAAS_MANIFEST_GIT_SHA}"

    # Stamp the VERSION file so the next run is idempotent.
    echo "${FAAS_MANIFEST_GIT_SHA}" > /opt/faas/current/VERSION
    echo "runbook-step-9: /opt/faas/current pinned to ${FAAS_MANIFEST_GIT_SHA}"
fi

# ADR-112: role templating. FAAS_BOX_ROLE is operator-supplied via
# cloud-init user-data; if it's the sentinel `__SET_BY_OPERATOR_AT_LAUNCH__`
# we already failed loud in first-boot.yaml.tpl's runcmd (assert-first-boot-env.sh
# exit 11). If it's missing entirely (no file), we also don't run
# (legacy boxes / dev images without the new template don't need this).
if [[ -n "${FAAS_BOX_ROLE:-}" ]] \
   && [[ "${FAAS_BOX_ROLE}" != "__SET_BY_OPERATOR_AT_LAUNCH__" ]]; then
    gregalectl release install --role="${FAAS_BOX_ROLE}"
    echo "runbook-step-9: applied role ${FAAS_BOX_ROLE}"
else
    echo "runbook-step-9: FAAS_BOX_ROLE unset or sentinel; skipping role templating (legacy/dev path)"
fi

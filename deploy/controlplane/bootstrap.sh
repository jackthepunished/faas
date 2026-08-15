#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────────────
# LEGACY — RETIRED 2026-08-15 by issue #911 / PR-1 (ADR-110).
#
# This script was the v1 control-plane installer. It is preserved as a
# tombstone so an operator who grep'd the repo for `bootstrap.sh` lands
# here instead of a 404. Re-running it is a no-op (it exits clean with
# code 64 EX_USAGE) but produces no useful state.
#
# v2 path (operator-side; PR-6.5 splits the operator verbs into
# `gregalectl`, leaving `gregale` for customers):
#   1. Run `make bootstrap`                                (ansible; deploy/ansible/bootstrap.yml)
#   2. Update deploy/manifest/splitbox.yaml with this host's DNS + role
#   3. Run `gregalectl manifest validate --manifest-file …`
#   4. Run `gregalectl manifest render  --manifest-file … --host <name>`
#   5. Run `gregalectl release install  --git-sha <sha> --node <name>`
#   6. Run `gregalectl host-age init|rotate`               (operator host.age)
#   7. Run `gregalectl pki init|status|rotate`             (local PKI bootstrap)
#   8. Run `gregalectl sign-keys init|rotate|status`       (cosign keypair)
#   9. Run `gregalectl node-key init|rotate|status`        (per-node signing key)
#  10. Run `gregalectl backup init` + `gregalectl trusted-publishers add …`
#
# The trusted-publishers verb still lives in `gregale` (admin API
# surface) — see plan §"Deviation". `gregale rollback` re-promotes
# a customer deployment; cluster rollback comes via
# `gregalectl release install --git-sha=<previous-sha>`.
# See:
#   docs/adr/110-declarative-split-box-manifest.md
#   PR-6.5 diff: <https://github.com/poyrazK/faas/pull/TBD>
# ─────────────────────────────────────────────────────────────────────
set -euo pipefail
echo "error: deploy/controlplane/bootstrap.sh retired by issue #911 / PR-1 (ADR-110)" >&2
echo "       use the v2 path: 'make bootstrap' + 'gregalectl manifest {validate,render}'" >&2
echo "       + 'gregalectl release install' + the operator secrets bootstraps" >&2
echo "       (host-age|pki|sign-keys|node-key|backup)" >&2
echo "       see docs/adr/110-declarative-split-box-manifest.md" >&2
exit 64

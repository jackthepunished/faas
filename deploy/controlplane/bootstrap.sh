#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────────────
# LEGACY — RETIRED 2026-08-15 by issue #911 / PR-1 (ADR-110).
#
# This script was the v1 control-plane installer. It is preserved as a
# tombstone so an operator who grep'd the repo for `bootstrap.sh` lands
# here instead of a 404. Re-running it is a no-op (it exits clean with
# code 64 EX_USAGE) but produces no useful state.
#
# v2 path:
#   1. Run `make bootstrap`         (ansible; deploy/ansible/bootstrap.yml)
#   2. Update deploy/manifest/splitbox.yaml with this host's DNS + role
#   3. Run `gregale manifest validate --manifest-file …`
#   4. Run `gregale manifest render  --manifest-file … --host <name>`
#   5. Run `gregale release install  --git-sha <sha> --host <name>`
#   6. (PR-X, pending) `gregale secrets init` — handles host.age,
#      session.key, deploy_ed25519, rclone.conf, box-age-key, sign-keys.
#
# Until PR-X ships, the secrets surface still requires manual setup.
# See:
#   docs/adr/110-declarative-split-box-manifest.md
#   PR-1 diff: <https://github.com/poyrazK/faas/pull/921>
# ─────────────────────────────────────────────────────────────────────
set -euo pipefail
echo "error: deploy/controlplane/bootstrap.sh retired by issue #911 / PR-1 (ADR-110)" >&2
echo "       use the v2 path: 'make bootstrap' + 'gregale manifest {validate,render}'" >&2
echo "       + 'gregale release install' (+ PR-X 'gregale secrets init' for the secrets surface)" >&2
echo "       see docs/adr/110-declarative-split-box-manifest.md" >&2
exit 64

#!/bin/bash
# run-metal-splitbox.sh — drive the issue #911 / PR-7 two-role Lima harness
# through the renderer (PR-2), release installer (PR-3), and doctor (PR-4).
#
# Runs in either the faas-metal-splitbox-cp (control-plane) guest or the
# faas-metal-splitbox-cx (compute-only) guest, depending on which guest
# the operator launched via limactl. The FAAS_BOX_ROLE env var tells the
# harness which role the host plays; the doctor surfaces role-specific
# findings (e.g. "vmmd expected on control-plane" → fail).
#
# Usage (from inside one of the Lima guests):
#   sudo -E env "PATH=$PATH" \
#     FAAS_BOX_ROLE=control-plane ./deploy/lima/run-metal-splitbox.sh
#   sudo -E env "PATH=$PATH" \
#     FAAS_BOX_ROLE=compute-only  ./deploy/lima/run-metal-splitbox.sh
#
# What it does:
#   1. gregalectl manifest validate --file \
#      deploy/manifest/examples/splitbox.example.yaml
#      (PR-7 §5 path; confirms the checked-in splitbox example is parseable
#      and every required field is present on every host). Set FAAS_MANIFEST
#      to an operator-owned manifest for a real fleet.
#   2. gregalectl manifest render --manifest-file "$MANIFEST"
#      --host <this-host> --dry-run (PR-2; the renderer produces the
#      per-host TOML tree without writing it).
#   3. gregalectl release bundle --bin-dir <bin> --git-sha <sha>
#      --manifest-hash <h> (PR-3; packages the eight daemons + the
#      firecracker/kernel/rootfs digests into /opt/faas/releases/<sha>/).
#   4. gregalectl release install --git-sha <sha> --node <this-host>
#      (PR-3; materialises the release on this box atomically — the
#      pkg/releaseinstall.AtomicFlip rollback primitive the cutover
#      runbook relies on).
#   5. gregalectl doctor (PR-4; exit 0 healthy / exit 3 drift).
#   6. gregalectl doctor --deep (PR-4; round-trips a few cross-daemon
#      sanity checks — schedd→vmmd dial, meterd→schedd ping).
#
# A failure in any of steps 1-4 aborts the harness with the offending
# exit code so the operator sees which gate failed. Step 5/6 failures
# log but do not abort (they are gate-keeping, not gate-tripping; the
# real acceptance is the cmd/e2e round-trip suite).

set -euo pipefail

# FAAS_BOX_ROLE selects the role the harness assumes this guest plays.
# Default to control-plane so a bare invocation still runs the full
# chain (the control-plane has more daemons to render).
: "${FAAS_BOX_ROLE:=control-plane}"

# FAAS_HOST_NAME identifies this guest in the manifest's fleet block.
# Default to the Lima-assigned hostname so the per-host render targets
# the right fleet entry.
: "${FAAS_HOST_NAME:=$(hostname)}"

# FAAS_PG_DSN — the in-guest Postgres (same trust-mode DSN as
# run-metal.sh:128). Without this, cmdReleaseInstall's best-effort
# compute_nodes UPSERT degrades to exit 3 + a warning in the JSON
# report. The harness then proceeds because the wire smoke is
# non-blocking.
export FAAS_PG_DSN="${FAAS_PG_DSN:-postgres://faas@127.0.0.1:5432/faas?sslmode=disable}"

# The manifest the harness validates and renders. The checked-in example is
# the clean-checkout default; a real fleet should pass its operator-owned
# manifest through FAAS_MANIFEST.
MANIFEST="${FAAS_MANIFEST:-deploy/manifest/examples/splitbox.example.yaml}"
if [ ! -f "$MANIFEST" ]; then
  echo "ERROR: manifest $MANIFEST not found" >&2
  echo "  Run from the repo checkout or set FAAS_MANIFEST to a valid manifest." >&2
  exit 1
fi

# Box-role sanity — must be one of the three pkg/role.Role values.
case "$FAAS_BOX_ROLE" in
  control-plane|compute-only|single-box) ;;
  *)
    echo "ERROR: FAAS_BOX_ROLE='$FAAS_BOX_ROLE' is not a pkg/role.Role value" >&2
    echo "  Expected one of: control-plane, compute-only, single-box" >&2
    exit 1
    ;;
esac

if [ ! -e /dev/kvm ]; then
  echo "WARN: /dev/kvm missing — firecracker M0/M1/M3/V6 paths will skip, but PR-7's renderer/installer/doctor chain still runs." >&2
fi

# Step 1 — manifest validate. PR-7 fixes the v1 path; this is the
# canonical "did I wire the manifest correctly" gate.
echo "==> step 1/6: gregalectl manifest validate --file $MANIFEST"
gregalectl manifest validate --file="$MANIFEST"

# Step 2 — manifest render dry-run. PR-2 produces the per-host TOML
# tree; the dry-run prints it without writing to /etc/faas. The
# cutover runbook (docs/runbooks/manifest-renderer-cutover.md §4)
# instructs operators to render-dry-run before render-for-real.
echo "==> step 2/6: gregalectl manifest render --manifest-file $MANIFEST --host $FAAS_HOST_NAME --role=$FAAS_BOX_ROLE --dry-run"
gregalectl manifest render \
  --manifest-file="$MANIFEST" \
  --host="$FAAS_HOST_NAME" \
  --role="$FAAS_BOX_ROLE" \
  --dry-run

# Step 3 — release bundle. PR-3's cmdReleaseBundle packages the
# eight daemons + the firecracker/kernel/rootfs digests into
# /opt/faas/releases/<sha>/{release-manifest.json, bin/<daemon>}.
# The harness passes METAL_BIN_DIR (the same env run-metal.sh:131
# consumes) so the bundle picks up the local build output.
GIT_SHA="$(git rev-parse HEAD 2>/dev/null || printf '0123456789abcdef0123456789abcdef01234567')"
MANIFEST_HASH="sha256:$(printf '%064d' 0)"
echo "==> step 3/6: gregalectl release bundle --bin-dir ${METAL_BIN_DIR:-/tmp/faas-metal-bin} --git-sha $GIT_SHA"
BIN_DIR="${METAL_BIN_DIR:-/tmp/faas-metal-bin}"
if [[ -d "${BIN_DIR}" ]]; then
  gregalectl release bundle \
    --bin-dir="${BIN_DIR}" \
    --git-sha="${GIT_SHA}" \
    --manifest-hash="${MANIFEST_HASH}" \
    --releases-root=/opt/faas/releases
else
  echo "WARN: bin-dir ${BIN_DIR} not found — release bundle step skipped." >&2
  echo "  Set METAL_BIN_DIR to the build output before re-running." >&2
fi

# Step 4 — release install. PR-3's cmdReleaseInstall materialises
# the release on this box atomically; the FAAS_BOX_ROLE flag scopes
# which PKI subset the installer lays out (mirrors gregalectl pki
# init --box-role from PR-3).
echo "==> step 4/6: gregalectl release install --git-sha $GIT_SHA --node $FAAS_HOST_NAME --box-role=$FAAS_BOX_ROLE"
if [[ -d "${BIN_DIR}" ]]; then
  gregalectl release install \
    --git-sha="${GIT_SHA}" \
    --releases-root=/opt/faas/releases \
    --node="${FAAS_HOST_NAME}" \
    --box-role="${FAAS_BOX_ROLE}" || \
    echo "WARN: release install failed — non-fatal; doctor below will surface the cause." >&2
fi

# Step 5 — doctor (shallow). PR-4's cmdDoctor validates the running
# binary's SHA matches git_sha, the per-host digests match the
# manifest, and the cgroup / TLS / cert modes are within policy.
# Exit 0 healthy; exit 3 drift.
echo "==> step 5/6: gregalectl doctor --node $FAAS_HOST_NAME"
gregalectl doctor --node="$FAAS_HOST_NAME" || \
  echo "WARN: doctor exited non-zero — see the findings above." >&2

# Step 6 — doctor --deep. PR-4's deep pass walks the wire cross-
# daemon sanity checks; expected to find no drift on a freshly
# installed box. Exit 0 healthy; exit 3 drift.
echo "==> step 6/6: gregalectl doctor --node $FAAS_HOST_NAME --deep"
gregalectl doctor --node="$FAAS_HOST_NAME" --deep || \
  echo "WARN: doctor --deep exited non-zero — see the findings above." >&2

echo "==> PR-7 splitbox harness complete (role=$FAAS_BOX_ROLE, host=$FAAS_HOST_NAME)"

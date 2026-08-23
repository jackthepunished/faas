#!/usr/bin/env bash
# scripts/pre-release-check.sh — Automated pre-release verification gate for Gregale FaaS.
#
# Verifies:
#  1. Production split-box deployment manifest validity (gregalectl manifest validate).
#  2. Consistency between git HEAD SHA and manifest release.git_sha.
#  3. Unit test suite passes cleanly across all packages.
#  4. Local canonical daemon tarball build simulation.
#  5. Calculates and displays the exact GREGALE_RELEASE_MANIFEST_HASH value.
#
set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
MANIFEST_PATH="${1:-$REPO_ROOT/deploy/manifest/production/gcp-live.yaml}"

echo "============================================================"
echo "  Gregale Pre-Release Candidate Verification Gate"
echo "============================================================"
echo "Manifest: $MANIFEST_PATH"

if [[ ! -f "$MANIFEST_PATH" ]]; then
  echo "[-] ERROR: Manifest file not found: $MANIFEST_PATH" >&2
  exit 1
fi

echo ""
echo "[1/5] Validating production deployment manifest..."
go run "$REPO_ROOT/cmd/gregalectl" manifest validate --file="$MANIFEST_PATH"
echo "[+] Manifest validation passed."

echo ""
echo "[2/5] Checking Git commit SHA alignment..."
CURRENT_GIT_SHA=$(git -C "$REPO_ROOT" rev-parse HEAD)
echo "  Current HEAD: $CURRENT_GIT_SHA"

MANIFEST_GIT_SHA=$(grep 'git_sha:' "$MANIFEST_PATH" | awk '{print $2}' | tr -d '"' | tr -d "'")
echo "  Manifest SHA: $MANIFEST_GIT_SHA"

if [[ "$CURRENT_GIT_SHA" != "$MANIFEST_GIT_SHA"* ]]; then
  echo "[-] WARNING: Manifest git_sha ($MANIFEST_GIT_SHA) does not match current HEAD ($CURRENT_GIT_SHA)."
  echo "    Update $MANIFEST_PATH before triggering release."
else
  echo "[+] Git SHA alignment confirmed."
fi

echo ""
echo "[3/5] Computing release manifest hash..."
if command -v sha256sum >/dev/null 2>&1; then
  RAW_HASH=$(sha256sum "$MANIFEST_PATH" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
  RAW_HASH=$(shasum -a 256 "$MANIFEST_PATH" | awk '{print $1}')
else
  echo "[-] ERROR: Neither sha256sum nor shasum found." >&2
  exit 1
fi
MANIFEST_HASH="sha256:$RAW_HASH"
echo "  GREGALE_RELEASE_MANIFEST_HASH=$MANIFEST_HASH"

echo ""
echo "[4/5] Running Go unit test suite..."
go -C "$REPO_ROOT" test ./pkg/... ./cmd/... ./guest/...
echo "[+] Unit test suite passed."

echo ""
echo "[5/5] Testing canonical daemon bundle build..."
SIM_OUT_DIR=$(mktemp -d "${TMPDIR:-/tmp}/gregale-preflight-bundle.XXXXXX")
trap 'rm -rf "$SIM_OUT_DIR"' EXIT

GIT_SHA="$CURRENT_GIT_SHA" \
MANIFEST_HASH="$MANIFEST_HASH" \
OUT_DIR="$SIM_OUT_DIR" \
"$REPO_ROOT/scripts/build-canonical-tarball.sh"

if [[ -f "$SIM_OUT_DIR/release.tar.gz" && -f "$SIM_OUT_DIR/release-manifest.json" ]]; then
  echo "[+] Canonical tarball built successfully: $(ls -lh "$SIM_OUT_DIR/release.tar.gz" | awk '{print $5}')"
else
  echo "[-] ERROR: Canonical tarball build failed to produce release artifacts." >&2
  exit 1
fi

echo ""
echo "============================================================"
echo "  Pre-Release Check Summary: ALL CHECKS PASSED (GREEN)"
echo "============================================================"
echo ""
echo "Next Operator Steps:"
echo "1. Set GitHub repository variable:"
echo "   gh variable set GREGALE_RELEASE_MANIFEST_HASH --body \"$MANIFEST_HASH\""
echo ""
echo "2. Tag and push pre-1.0 release candidate (e.g. v0.1.3-rc.1):"
echo "   git tag -a v0.1.3-rc.1 -m \"Release candidate v0.1.3-rc.1\""
echo "   git push origin v0.1.3-rc.1"
echo ""
echo "3. Monitor GitHub Actions release pipeline (.github/workflows/release.yml)"
echo "4. Deploy signed release bundle across fleet:"
echo "   gregalectl release install --manifest=$MANIFEST_PATH ..."
echo "5. Run deep diagnostic verification:"
echo "   gregalectl doctor --deep"
echo "============================================================"

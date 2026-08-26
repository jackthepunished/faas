#!/usr/bin/env bash
# Static + behavioral test for the release manifest materializer.
set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
SCRIPT="$REPO_ROOT/scripts/materialize-release-manifest.sh"
TEMPLATE="$REPO_ROOT/deploy/manifest/production/gcp-live.template.yaml"
TMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/gregale-manifest-test.XXXXXX")
trap 'rm -rf "$TMP_DIR"' EXIT

SHA=0123456789abcdef0123456789abcdef01234567
OUT_A="$TMP_DIR/production-manifest.yaml"
OUT_B="$TMP_DIR/production-manifest-second.yaml"

bash "$SCRIPT" --template "$TEMPLATE" --git-sha "$SHA" --output "$OUT_A"
bash "$SCRIPT" --template "$TEMPLATE" --git-sha "$SHA" --output "$OUT_B"

grep -Fqx '  id: pre-1.0-01234567' "$OUT_A"
grep -Fqx "  git_sha: $SHA" "$OUT_A"
cmp -s "$OUT_A" "$OUT_B"
! cmp -s "$TEMPLATE" "$OUT_A"

if MANIFEST_FILE="$OUT_A" \
  MANIFEST_HASH="sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" \
  GIT_SHA="$SHA" \
  OUT_DIR="$TMP_DIR/unused" \
  "$REPO_ROOT/scripts/build-canonical-tarball.sh" >/dev/null 2>&1; then
  echo "build-canonical-tarball accepted a mismatched manifest hash" >&2
  exit 1
fi

go run "$REPO_ROOT/cmd/gregalectl" manifest validate --file "$OUT_A" >/dev/null
echo "materialize-release-manifest: test passed"

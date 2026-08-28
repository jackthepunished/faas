#!/usr/bin/env bash
# Materialize the immutable release identity into a production manifest.
#
# The checked-in production manifest is a topology template. Release CI
# writes the tag commit into its release tuple and publishes the resulting
# bytes with the signed daemon bundle. Keeping this projection out of git
# avoids the impossible self-reference of a commit containing its own SHA.
set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
TEMPLATE="$REPO_ROOT/deploy/manifest/production/gcp-live.template.yaml"
GIT_SHA=""
OUTPUT=""
BUILDER_BASE_DIGEST=""

usage() {
  cat >&2 <<'USAGE'
usage: materialize-release-manifest.sh --git-sha SHA --output PATH [--template PATH] [--builder-base-digest DIGEST]
USAGE
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --template)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      TEMPLATE=$2
      shift 2
      ;;
    --git-sha)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      GIT_SHA=$2
      shift 2
      ;;
    --output)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      OUTPUT=$2
      shift 2
      ;;
    --builder-base-digest)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      BUILDER_BASE_DIGEST=$2
      shift 2
      ;;
    -h|--help)
      usage >&2
      exit 0
      ;;
    *)
      usage
      exit 2
      ;;
  esac
done

if [[ ! "$GIT_SHA" =~ ^[0-9a-f]{40}$ ]]; then
  echo "materialize-release-manifest: --git-sha must be a 40-character lowercase SHA" >&2
  exit 2
fi
if [[ -z "$OUTPUT" ]]; then
  echo "materialize-release-manifest: --output is required" >&2
  exit 2
fi
if [[ ! -f "$TEMPLATE" ]]; then
  echo "materialize-release-manifest: template not found: $TEMPLATE" >&2
  exit 1
fi
if [[ -n "$BUILDER_BASE_DIGEST" && ! "$BUILDER_BASE_DIGEST" =~ ^(sha256:)?[0-9a-f]{64}$ ]]; then
  echo "materialize-release-manifest: --builder-base-digest must be a sha256 digest" >&2
  exit 2
fi
BUILDER_BASE_DIGEST="${BUILDER_BASE_DIGEST#sha256:}"

mkdir -p "$(dirname "$OUTPUT")"
tmp=$(mktemp "${OUTPUT}.tmp.XXXXXX")
trap 'rm -f "$tmp"' EXIT

release_id="pre-1.0-${GIT_SHA:0:8}"
awk -v release_id="$release_id" -v release_sha="$GIT_SHA" \
  -v builder_base_digest="$BUILDER_BASE_DIGEST" '
  function fail(message) {
    print "materialize-release-manifest: " message > "/dev/stderr"
    exit 1
  }
  BEGIN {
    in_release = 0
    release_sections = 0
    release_ids = 0
    release_shas = 0
    builder_base_digests = 0
  }
  /^release:[[:space:]]*$/ {
    if (in_release || release_sections != 0) {
      fail("template must contain exactly one release section")
    }
    in_release = 1
    release_sections++
    print
    next
  }
  in_release && /^  id:[[:space:]]/ {
    print "  id: " release_id
    release_ids++
    next
  }
  in_release && /^  git_sha:[[:space:]]/ {
    print "  git_sha: " release_sha
    release_shas++
    next
  }
  in_release && builder_base_digest != "" && /^  builder_base_digest:[[:space:]]/ {
    print "  builder_base_digest: " builder_base_digest
    builder_base_digests++
    next
  }
  in_release && /^[^[:space:]#]/ {
    in_release = 0
  }
  { print }
  END {
    if (release_sections != 1 || release_ids != 1 || release_shas != 1) {
      fail("template must contain one indented release.id and release.git_sha")
    }
    if (builder_base_digest != "" && builder_base_digests != 1) {
      fail("template must contain one indented release.builder_base_digest when an override is supplied")
    }
  }
' "$TEMPLATE" > "$tmp"

mv "$tmp" "$OUTPUT"
trap - EXIT
chmod 0644 "$OUTPUT"
printf 'materialized %s (release=%s, manifest hash computed by caller)\n' "$OUTPUT" "$release_id"

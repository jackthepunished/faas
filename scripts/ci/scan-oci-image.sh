#!/usr/bin/env bash
# scan-oci-image.sh — scan one concrete OCI image artifact.
#
# The source prefix is deliberately explicit: `docker` scans a locally loaded
# image and `registry` scans an image served by a registry. A tag is accepted
# only because the CI callers use immutable PR/SHA tags; production release
# manifests still carry the resolved digest.

set -euo pipefail

source_kind=${1:?source kind is required (docker or registry)}
image_ref=${2:?image reference is required}
platform=${3:-linux/amd64}
fail_on=${GRYPE_FAIL_ON:-critical}
grype_bin=${GRYPE_BIN:-grype}

case "${source_kind}" in
  docker|registry) ;;
  *)
    echo "source kind must be docker or registry, got ${source_kind}" >&2
    exit 2
    ;;
esac

"${grype_bin}" "${source_kind}:${image_ref}" \
  --platform "${platform}" \
  --fail-on "${fail_on}" \
  --only-fixed=false \
  -o table

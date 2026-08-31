#!/usr/bin/env bash
set -euo pipefail

image_ref=${1:?image reference is required}
runtime_image=${2:?runtime image name is required}

case "${runtime_image}" in
  base-debian-parent)
    required=(etc/passwd bin/sh)
    ;;
  base-minimal)
    required=(etc/passwd bin/busybox bin/sh)
    ;;
  runner-node22|runner-node24)
    required=(etc/passwd usr/local/bin/node)
    ;;
  runner-python312|runner-python313)
    required=(etc/passwd usr/local/bin/python3)
    ;;
  runner-go124|runner-go124-alpine)
    required=(etc/passwd usr/local/go/bin/go)
    ;;
  *)
    echo "unsupported runtime image ${runtime_image}" >&2
    exit 2
    ;;
esac

container_id=$(docker create --entrypoint /bin/sh "${image_ref}" -c true)
rootfs_tar=""
rootfs_listing=""
cleanup() {
  docker rm -f "${container_id}" >/dev/null 2>&1 || true
  if [[ -n "${rootfs_tar}" ]]; then
    rm -f "${rootfs_tar}"
  fi
  if [[ -n "${rootfs_listing}" ]]; then
    rm -f "${rootfs_listing}"
  fi
}
trap cleanup EXIT

rootfs_tar=$(mktemp)
docker export "${container_id}" -o "${rootfs_tar}"
rootfs_listing=$(mktemp)
# Materialise the listing before matching. Piping tar into grep -q under
# pipefail makes tar receive SIGPIPE as soon as grep finds a match, which
# falsely turns a present path into a missing-path failure.
tar -tf "${rootfs_tar}" >"${rootfs_listing}"
for path in "${required[@]}"; do
  if ! grep -Eq "^(\./)?${path}(/|$)" "${rootfs_listing}"; then
    echo "::error::${image_ref} is missing /${path}" >&2
    exit 1
  fi
done
echo "OK: ${image_ref} contains the ${runtime_image} rootfs contract"

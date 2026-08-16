#!/usr/bin/env bash
# scripts/install-packer.sh — install the pinned Packer version (ADR-111).
#
# ADR-111 design: the image build pipeline depends on a Packer binary whose
# version is content-addressed by the tag. apt's packer is too new (1.11+) and
# plugin compat isn't guaranteed; we curl + sha256 + tar from the official
# release page instead (same pattern as deploy/lima/* + scripts/install-*).
#
# Usage: scripts/install-packer.sh [VERSION]
#   Default VERSION is read from deploy/packer/Makefile:PACKER_VERSION.
#
# Installs to /usr/local/bin/packer (sudo if not root).
set -euo pipefail

VERSION="${1:-1.10.0}"
SHA="cb14c061888195dc834d4c0e57a7669b8e15b51b94bfa9c50e3bf5a6227c764e"
URL="https://releases.hashicorp.com/packer/${VERSION}/packer_${VERSION}_linux_amd64.zip"

if command -v packer >/dev/null 2>&1; then
    HAVE_VERSION="$(packer --version)"
    if [[ "${HAVE_VERSION}" == "${VERSION}" ]]; then
        echo "packer ${VERSION} already installed — skipping"
        exit 0
    fi
    echo "packer ${HAVE_VERSION} present, want ${VERSION} — replacing"
fi

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

echo "Downloading packer ${VERSION}…"
curl --fail --silent --show-error --location --retry 3 --retry-delay 2 \
    -o "${TMP}/packer.zip" "${URL}"

echo "${SHA}  ${TMP}/packer.zip" | sha256sum --check --strict

echo "Unpacking…"
(cd "${TMP}" && unzip -q packer.zip)

INSTALL_DIR=/usr/local/bin
if [[ ! -w "${INSTALL_DIR}" ]]; then
    SUDO=sudo
fi
${SUDO:-} mv "${TMP}/packer" "${INSTALL_DIR}/packer"
${SUDO:-} chmod 0755 "${INSTALL_DIR}/packer"

packer --version

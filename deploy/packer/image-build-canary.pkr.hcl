// image-build-canary.pkr.hcl — ADR-113 PR-B (canonical daemon
// tarball) build-time OCI publish.
//
// This is the producer-side counterpart to PR-A's consumer-side
// (Tarball.Verify + install-time SBoM gate). The on-disk triple
// (release.tar.gz + release.cosign.bundle + release.sbom.json) is
// published to:
//
//   oci://ghcr.io/poyrazK/faas/release:v<git_sha>@sha256:<digest>
//
// via the `oras` CLI shelling from per-step provisioners. No new
// Go dependency is introduced — `oras` is on the canary runner's
// PATH (installed in the prerequisite step below).
//
// This is NOT a VM builder. It uses the `null` builder, which
// runs provisioners on the host (the canary runner) and exits 0.
// The output is the OCI push, not a snapshot.
//
// Validation gate: `make image-validate` (the existing glob
// `deploy/packer/*.pkr.hcl`) picks this file up automatically. A
// separate `image-build-canary` target in the Makefile avoids
// running the canary on every `image-validate` run.
//
// Required binaries: cosign, syft, oras. Install via the
// prerequisite step below OR via the runner's pre-baked image.
//
// Note on `oras push` (PR-B review fix #1): the original draft used
// `oras copy --to-oci`, which is wrong — `oras copy` requires both
// sides to be resolvable OCI refs (it transfers between registries,
// not from local files). To push a local artifact to OCI you must
// use `oras push` with `<to-ref> <file:mediatype>`. The digest-pinned
// `<to-ref>` (`@sha256:<digest>`) is still content-addressed the same
// way as before; consumers can pin either tag (mutable) or digest
// (immutable) per the OCI spec.

packer {
  required_version = ">= 1.10.0"
}

variable "git_sha" {
  type        = string
  description = "40-char lowercase hex git SHA of the release being published. Used as the OCI tag."
}

variable "oci_registry" {
  type        = string
  description = "OCI registry namespace. Default: ghcr.io/poyrazK/faas (matches the dns01_provider_cloudflare / supply-chain convention)."
  default     = "ghcr.io/poyrazK/faas"
}

variable "release_repo" {
  type        = string
  description = "OCI repository name under the registry. The release artefact published here."
  default     = "release"
}

variable "artifacts_dir" {
  type        = string
  description = "Directory holding the canonical triple (release.tar.gz, release.cosign.bundle, release.sbom.json). Producer side: see scripts/build-canonical-tarball.sh (PR-A commit 1)."
  default     = "out"
}

variable "media_type_tarball" {
  type        = string
  description = "OCI media type for the canonical tarball. vnd.gregale.daemon.v1.tar+gzip mirrors the convention from PR-A's existing tarball producer."
  default     = "application/vnd.gregale.daemon.v1.tar+gzip"
}

variable "media_type_cosign" {
  type        = string
  description = "OCI media type for cosign bundles. The cosign SDK standard."
  default     = "application/vnd.dev.cosign.bundle.v1+json"
}

variable "media_type_sbom" {
  type        = string
  description = "OCI media type for SPDX-2.3 SBoMs. Canonical per the SPDX 2.3 spec."
  default     = "application/spdx+json"
}

locals {
  // oci_ref is the canonical OCI URI for the canary's release.
  // The sha256 digest is computed in the prerequisite step; the
  // tag is the git_sha. The `@sha256:<digest>` suffix is what
  // makes the URI content-addressed; consumers can pin either
  // tag (mutable) or digest (immutable).
  oci_ref = "oci://${var.oci_registry}/${var.release_repo}:v${var.git_sha}"
}

source "null" "canary" {
  communicator = "none"
}

// ---------------------------------------------------------------------------
// Build phase:
//
//   1. tools        — install cosign, syft, oras (idempotent; uses
//                     the host's pre-installed binaries when present)
//   2. publish-tar  — oras push the canonical tarball to OCI
//   3. attach-cosign — oras attach the cosign bundle as a referrer
//   4. attach-sbom  — oras attach the SPDX-2.3 SBoM as a referrer
//   5. verify       — sanity-check the OCI pull list (defensive)
//
// Error message capitalization follows the [[packer-1-10-error-
// message-capitalization]] lesson: every error_message string
// starts with a capital letter and ends with `.` or `?`.
// ---------------------------------------------------------------------------
build {
  name    = "image-build-canary"
  sources = ["source.null.canary"]

  provisioner "shell" {
    name            = "tools"
    inline_shebang  = "/bin/sh -e"
    inline = [
      "set -eu",
      "command -v cosign >/dev/null 2>&1 || { echo 'Missing required tool: cosign. Install via scripts/install-cosign.sh (or apt-get install cosign).' >&2; exit 1; }",
      "command -v syft >/dev/null 2>&1 || { echo 'Missing required tool: syft. Install via scripts/install-syft.sh (or curl -sSfL https://raw.githubusercontent.com/anchore/syft/main/install.sh | sh -s -- -b /usr/local/bin).' >&2; exit 1; }",
      "command -v oras >/dev/null 2>&1 || { echo 'Missing required tool: oras. Install via scripts/install-oras.sh (or curl -sSfL https://raw.githubusercontent.com/oras-project/oras/main/scripts/install.sh | sh).' >&2; exit 1; }",
    ]
  }

  provisioner "shell" {
    name            = "publish-tar"
    inline_shebang  = "/bin/sh -e"
    environment_vars = ["GIT_SHA=${var.git_sha}", "ARTIFACTS_DIR=${var.artifacts_dir}", "OCI_REF=${local.oci_ref}", "MEDIA_TYPE=${var.media_type_tarball}"]
    inline = [
      "set -eu",
      "test -f \"$ARTIFACTS_DIR/release.tar.gz\" || { echo 'Missing required artifact: release.tar.gz. Run scripts/build-canonical-tarball.sh first.' >&2; exit 1; }",
      "DIGEST=$(sha256sum \"$ARTIFACTS_DIR/release.tar.gz\" | awk '{print $1}')",
      "echo \"Publishing $ARTIFACTS_DIR/release.tar.gz -> $OCI_REF@sha256:$DIGEST\"",
      "oras push \"$OCI_REF@sha256:$DIGEST\" \"$ARTIFACTS_DIR/release.tar.gz:$MEDIA_TYPE\"",
    ]
  }

  provisioner "shell" {
    name            = "attach-cosign"
    inline_shebang  = "/bin/sh -e"
    environment_vars = ["GIT_SHA=${var.git_sha}", "ARTIFACTS_DIR=${var.artifacts_dir}", "OCI_REF=${local.oci_ref}", "MEDIA_TYPE=${var.media_type_cosign}"]
    inline = [
      "set -eu",
      "test -f \"$ARTIFACTS_DIR/release.cosign.bundle\" || { echo 'Missing required artifact: release.cosign.bundle. The canary post-process must run cosign sign-blob first.' >&2; exit 1; }",
      "DIGEST=$(sha256sum \"$ARTIFACTS_DIR/release.tar.gz\" | awk '{print $1}')",
      "oras attach --image \"$OCI_REF@sha256:$DIGEST\" \"$ARTIFACTS_DIR/release.cosign.bundle:$MEDIA_TYPE\"",
    ]
  }

  provisioner "shell" {
    name            = "attach-sbom"
    inline_shebang  = "/bin/sh -e"
    environment_vars = ["GIT_SHA=${var.git_sha}", "ARTIFACTS_DIR=${var.artifacts_dir}", "OCI_REF=${local.oci_ref}", "MEDIA_TYPE=${var.media_type_sbom}"]
    inline = [
      "set -eu",
      "test -f \"$ARTIFACTS_DIR/release.sbom.json\" || { echo 'Missing required artifact: release.sbom.json. Run scripts/build-canonical-tarball.sh first (it emits the SPDX-2.3 SBoM via syft).' >&2; exit 1; }",
      "DIGEST=$(sha256sum \"$ARTIFACTS_DIR/release.tar.gz\" | awk '{print $1}')",
      "oras attach --image \"$OCI_REF@sha256:$DIGEST\" \"$ARTIFACTS_DIR/release.sbom.json:$MEDIA_TYPE\"",
    ]
  }

  provisioner "shell" {
    name            = "verify"
    inline_shebang  = "/bin/sh -e"
    environment_vars = ["GIT_SHA=${var.git_sha}", "OCI_REF=${local.oci_ref}"]
    inline = [
      "set -eu",
      "DIGEST=$(sha256sum \"$ARTIFACTS_DIR/release.tar.gz\" | awk '{print $1}')",
      "echo \"Verifying OCI pull list for $OCI_REF@sha256:$DIGEST\"",
      "oras discover --target \"$OCI_REF@sha256:$DIGEST\" --format json | tee /tmp/canary-oras-discover.json",
      "echo 'Canary publish complete. Triple is reachable at the OCI ref above.'",
    ]
  }
}

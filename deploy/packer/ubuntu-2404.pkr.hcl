// ubuntu-2404.pkr.hcl — base Ubuntu 24.04 LTS source definition.
//
// ADR-111 + §14 M0 acceptance bar:
//   - Base OS = Ubuntu 24.04 LTS (long-term-support, kernel 6.8 HWE).
//   - Same OS on every cloud (hcloud + AWS + bare-metal EX44) — the image
//     must boot without per-cloud base-OS surprises.
//   - Per-cloud builders source this file and override cloud-specific
//     fields (source_ami_id for AWS, source_image for hcloud, etc.).
//
// Common.pkr.hcl defines the variables + locals; this file declares the
// canonical base source shape that per-cloud builders consume.
//
// `packer validate -syntax-only` MUST pass on this file with no cloud creds:
// the source block is declared but no actual build is invoked.

packer {
  required_version = ">= 1.10.0"
}

// ---------------------------------------------------------------------------
// Per-cloud source-args. Each cloud builder sources this file and supplies
// the cloud-specific source image / ami / iso. We declare variables for the
// per-cloud specifics and require them at the per-cloud-builder level (not
// here, since this file is OS-only).
// ---------------------------------------------------------------------------
variable "ubuntu_release" {
  type        = string
  description = "Ubuntu release codename. Pinned to noble (24.04 LTS) per ADR-111."
  default     = "noble"
}

variable "ubuntu_arch" {
  type        = string
  description = "Architecture for the base image. Pinned to x86_64 — production control-plane nodes are x86_64 per CLAUDE.md."
  default     = "amd64"

  validation {
    condition     = var.ubuntu_arch == "amd64"
    error_message = "Ubuntu_arch must be amd64; arm64 nested-virt Lima boxes do NOT produce production images (see deploy/lima/README.md caveat)."
  }
}

// ---------------------------------------------------------------------------
// Source: the canonical Ubuntu 24.04 LTS source definition. Per-cloud
// builders use this as their base and override platform-specific fields:
//
//   hcloud:
//     source_image = "ubuntu-24.04"
//   amazon-ebs:
//     source_ami_filter { ... name = "ubuntu/images/hvm-ssd/ubuntu-jammy-24.04-amd64-server-*" ... }
//   iso (bare-metal):
//     iso_url = "https://releases.ubuntu.com/24.04/ubuntu-24.04.2-live-server-amd64.iso"
//
// This file doesn't declare an active source block — the per-cloud builders
// do. We declare the variables that SOURCE those values (so the validation
// gate can sanity-check the variables before the cloud builder runs).
// ---------------------------------------------------------------------------
locals {
  // Canonical base image name per cloud. Used by per-cloud builders to
  // distinguish their source from one another.
  ubuntu_base_release = "${var.ubuntu_release}-${var.ubuntu_arch}"
}

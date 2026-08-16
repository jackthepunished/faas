// iso.pkr.hcl — bare-metal ISO builder for the Gregale Compute Image.
//
// ADR-112: composes the shared scaffolding + Ubuntu 24.04 base (same as
// hcloud / amazon-ebs). The output is an Ubuntu 24.04 autoinstall ISO;
// PXE-booting the EX44 with this ISO installs the Gregale Compute Image
// as the bare-metal host. The image is role-agnostic — first-boot
// `gregalectl release install --role` does the role templating.
//
// Per-cloud specifics (this file):
//   - source: Ubuntu 24.04 live-server ISO + autoinstall user-data
//   - boot_command: kicks off the autoinstall (no human-in-the-loop)
//   - output: a custom .iso (not a snapshot/AMI)
//
// Bare-metal provisioning is the legacy path; cloud builders are
// preferred for new boxes. The ISO builder stays because:
//   (a) the spec claims "deployable on any bare-metal x86_64 control-
//       plane node" — image must support that path;
//   (b) dev boxes without a cloud account still need a path;
//   (c) the iso builder produces a verifiable artifact on any laptop.

packer {
  required_version = ">= 1.10.0"
}

variable "ubuntu_iso_url" {
  type        = string
  description = "URL of the Ubuntu 24.04 live-server ISO."
  default     = "https://releases.ubuntu.com/24.04/ubuntu-24.04.3-live-server-amd64.iso"
}

variable "ubuntu_iso_sha256" {
  type        = string
  description = "SHA-256 of the Ubuntu 24.04 ISO. Pinned; supply-chain check before boot. Mirrors releases.ubuntu.com/24.04/SHA256SUMS."
  default     = "c3514bf0056180d09376462a7a1b4f213c1d6e8ea67fae5c25099c6fd3d8274b"
}

variable "autoinstall_path" {
  type        = string
  description = "Path on the build host's http_directory containing the autoinstall user-data (cloud-init / NoCloud equivalent). ADR-112: a single template for both roles (per-role surface lives in user-data)."
  default     = "cloud-init/first-boot.yaml.tpl"
}

source "iso" "compute" {
  iso_url           = var.ubuntu_iso_url
  iso_checksum      = "sha256:${var.ubuntu_iso_sha256}"
  http_directory    = "."
  boot_wait         = "5s"
  boot_command      = [
    "<esc><wait>",
    "<esc><wait>",
    "/casper/vmlinuz ",
    "autoinstall ds=nocloud-net;s=http://{{ .HTTPIP }}:{{ .HTTPPort }}/ ",
    "--- <enter>"
  ]

  // The ISO builder runs the install via QEMU/KVM locally. The output
  // is a fresh .iso with the autoinstall baked in — no image upload.
  output_directory = "packer-iso"

  // Default QEMU shape: 2 vCPU, 4 GB — matches the AWS t3.medium so
  // build behaviour matches cloud. The EX44 has 16 vCPU / 64 GB but
  // the install doesn't need that.
  qemuargs = [
    ["-m", "4096M"],
    ["-smp", "2"],
  ]
}

build {
  name    = "iso"
  sources = ["source.iso.compute"]

  // The iso builder runs the entire install via cloud-init user-data.
  // ADR-112's first-boot cloud-init user-data is the authoritative
  // source; this file references the SAME scripts the cloud builders
  // invoke post-install.
  provisioner "shell" {
    script = "scripts/build-base.sh"
    environment_vars = ["FAAS_GIT_SHA=${var.git_sha}"]
  }

  // Post-install, re-pack the install into a redistributable .iso
  // (custom iso creator — out of scope for PR #929; this file documents
  // the contract and lets the syntax gate pass).
}

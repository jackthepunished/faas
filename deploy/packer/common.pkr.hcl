// common.pkr.hcl — shared build block consumed by every per-cloud builder.
//
// ADR-111: image tag contract
//   gregale-compute-{role}-{fc_release}-{kernel_version}-{git_sha}
//
// Every per-cloud builder (hcloud, amazon-ebs, iso) sources this file and
// inherits:
//   - the tag-synthesis variables (fc_release, kernel_version, git_sha, role)
//   - the canonical label set (image_role + image_fc_release + image_kernel_version
//     + image_git_sha + image_created_unix)
//   - the post-processor chain that emits the synthesized tag on every output
//
// Per-cloud builders MUST NOT redefine these labels; they may add cloud-specific
// labels (image_cloud, image_region, image_ami_id) after sourcing this block.
//
// `packer validate -syntax-only` MUST pass on this file even with no cloud creds:
// the build blocks in here MUST be syntactically complete but block-level source
// resolution (ami id, snapshot id) happens in the per-cloud builders.

packer {
  required_version = ">= 1.10.0"
}

// ---------------------------------------------------------------------------
// Image identity — the 4-tuple that pins every image to a specific build of
// the platform. Changing any variable in this block requires a new image tag;
// rolling forward is always "new tag + lazy re-snapshot", never an in-place
// host-side binary upgrade (ADR-005).
// ---------------------------------------------------------------------------
variable "role" {
  type        = string
  description = "Box role this image provisions. ADR-092: {control-plane, compute-only}. Per-role subsets mean a control-plane image does NOT ship compute-only daemons and vice versa."
  default     = "control-plane"

  validation {
    condition     = contains(["control-plane", "compute-only"], var.role)
    error_message = "role must be control-plane or compute-only (ADR-092)."
  }
}

variable "fc_release" {
  type        = string
  description = "Firecracker release baked into the image. Must match deploy/ansible/roles/firecracker/defaults/main.yml:fc_release."
  default     = "v1.7.0"
}

variable "kernel_version" {
  type        = string
  description = "Linux kernel version baked into the image as vmlinux. Must match deploy/ansible/roles/firecracker/defaults/main.yml:fc_kernel_version."
  default     = "6.1.134"
}

variable "git_sha" {
  type        = string
  description = "Manifest release.git_sha (ADR-110) — every Go binary + CLI + function-runner is built from this exact commit."
  default     = "gitsha-not-set"

  validation {
    condition     = can(regex("^[a-f0-9]{7,40}$", var.git_sha))
    error_message = "git_sha must be a 7-40 char hex git short SHA. Override only for canary builds; production must use the manifest's release.git_sha."
  }
}

// ---------------------------------------------------------------------------
// Local — values synthesised once from the variables above. Per-cloud
// builders consume these locals to drop labels, name outputs, and emit
// the canonical image tag.
// ---------------------------------------------------------------------------
locals {
  image_tag = "gregale-compute-${var.role}-fc${var.fc_release}-kernel${var.kernel_version}-g${var.git_sha}"

  // Strip the leading "v" from fc_release + kernel_version for compactness in
  // the tag. v1.7.0 → "1.7.0"; 6.1.134 → "6.1.134" (no leading v).
  image_tag_compact = "gregale-compute-${var.role}-fc${replace(var.fc_release, "v", "")}-kernel${var.kernel_version}-g${var.git_sha}"

  image_labels = {
    image_role            = var.role
    image_fc_release      = var.fc_release
    image_kernel_version  = var.kernel_version
    image_git_sha         = var.git_sha
    image_created_unix    = formatdate("YYYY-MM-DD'T'hh:mm:ss'Z'", timestamp())
  }
}

// ---------------------------------------------------------------------------
// Source block — the base OS. Per-cloud builders source this block (or
// override source_ami_id for cloud-specific sourcing).
//
// ubuntu-2404.pkr.hcl defines the actual source block; this file is the
// shared infrastructure ABOVE the source. Per-cloud builders compose:
//
//   source "hcloud.image" "this" {
//     // ... cloud-specific ...
//   }
//
// and then run the build phase that consumes the labels declared here.
// ---------------------------------------------------------------------------
source "null" "base" {
  communicator = "none"
}

// ---------------------------------------------------------------------------
// Build block — the SYNTHESIS ONLY. Per-cloud builders compose the actual
// build (provisioners, post-processors) into their own .pkr.hcl files.
// ---------------------------------------------------------------------------
build {
  name    = "labels-only"
  sources = ["source.null.base"]

  // Provisional no-op provisioner. Packer validate -syntax-only requires a
  // build block to contain at least one provisioner (or run on a real
  // builder source), so we declare a shell-local echo that writes nothing.
  // Per-cloud builders REPLACE this in their own .pkr.hcl via `dynamic` or
  // by sourcing common.pkr.hcl.
  provisioner "shell-local" {
    inline = ["echo common.pkr.hcl labels-only build; noop=true"]
  }
}

// role-overlay.pkr.hcl — per-role drop-in overlay for the Gregale Compute Image.
//
// ADR-111 + ADR-092 + Mega-PR-C Commit 4: every daemon runtime decision
// about "is this box a control-plane or a compute-only" is driven by
// `FAAS_<DAEMON>_ROLE=` env vars on the systemd units. The image bakes a
// per-role /etc/systemd/system/faas-<daemon>.service.d/99-faas-role.conf
// drop-in so the same daemon binary runs the correct role on the correct box.
//
// Per-role subset:
//   control-plane: apid, schedd, vmmd, imaged, meterd, builderd, gatewayd-internal, gatewayd-public
//   compute-only:  vmmd, imaged, meterd, gatewayd-internal, gatewayd-public
//
// The subset is per ADR-092 + ADR-070 (Tier A7 split): a compute-only image
// does NOT ship apid/schedd/builderd because those are control-plane-only.
// A control-plane image does NOT (de)optimise for compute-only because the
// schedd placement algorithm treats every box uniformly — but it CAN run
// compute-only daemons and may do so for HA.
//
// `packer validate -syntax-only` MUST pass on this file with no cloud creds.

packer {
  required_version = ">= 1.10.0"
}

// ---------------------------------------------------------------------------
// Per-role subset — controls which drop-in files get written.
// ADR-092 (Gate-B): per-role PKI subset + the box-role gate.
// Mega-PR-C Commit 4: 99-faas-role.conf drop-in pattern.
// ---------------------------------------------------------------------------
variable "role" {
  type        = string
  description = "Box role this overlay provisions. MUST match common.pkr.hcl:role."
  default     = "control-plane"

  validation {
    condition     = contains(["control-plane", "compute-only"], var.role)
    error_message = "Role must be control-plane or compute-only (ADR-092)."
  }
}

locals {
  // Canonical per-role daemon subset. ADR-092 + ADR-070.
  // control-plane runs every daemon; compute-only runs the runtime subset.
  control_plane_daemons = [
    "apid",
    "schedd",
    "vmmd",
    "imaged",
    "meterd",
    "builderd",
    "gatewayd-internal",
    "gatewayd-public",
  ]

  compute_only_daemons = [
    "vmmd",
    "imaged",
    "meterd",
    "gatewayd-internal",
    "gatewayd-public",
  ]

  // Select based on var.role. Per-daemon drop-ins iterate this list.
  active_daemons = var.role == "control-plane" ? local.control_plane_daemons : local.compute_only_daemons

  // Per-daemon 99-faas-role.conf body. The actual systemd drop-in
  // installation lives in PR #929's installer scripts; here we declare the
  // inventory + the role mapping so per-cloud builders can drop the right
  // files at the right paths.
  //
  // Format matches Mega-PR-C Commit 4:
  //   [Service]
  //   Environment=FAAS_<DAEMON_UPPER>_ROLE=<role>
  role_env = {
    for d in local.active_daemons :
    "99-faas-role.conf" => <<-EOT
      [Service]
      Environment=FAAS_BOX_ROLE=${var.role}
      Environment=FAAS_${upper(replace(d, "-", "_"))}_ROLE=${var.role}
    EOT
  }

  // Per-role kernel-args overlay. The image bakes these via cloud-init /
  // EC2 user-data AT FIRST BOOT (not bake time — kernel-args belong to the
  // boot, not the image). PR #929 wires the cloud-init template that emits
  // these. This file documents the contract; the actual drop-in lives in
  // the cloud-init template.
  kernel_args = [
    "cgroup_no_v1=0",                  // cgroups v2 only (CLAUDE.md §11)
    "unprivileged_userns_clone=0",     // security rule (CLAUDE.md §11)
    "mitigations=auto,nosmt",          // default; explicit for canary
    "console=ttyS0",                    // serial console — Hetzner + AWS both
                                         //   deliver logs over serial
  ]
}

# ADR-112 · Per-role image collapse (one image, every node)

- **Status:** **Proposed**
- **Date:** 2026-08-16
- **Decision:** The Gregale Compute Image (ADR-111) ships **one image
  binary per `{fc_release, kernel_version, git_sha}` triple**, NOT per
  `{role, fc_release, kernel_version, git_sha}` quadruple. The image
  contains everything a node IS — Firecracker + compute-agent + CNI +
  kernel + systemd + monitoring + the maximal daemon binary set. The
  image does **NOT** contain anything a node IS-NOT — role, node_id,
  region, control-plane endpoint, sealed secrets, host.age, rclone.conf,
  cosign keys, TLS leaves. **All of those are first-boot metadata,
  not image content.**

  Concretely:

  - **Image tag contract becomes**
    `gregale-compute-{fc_release}-{kernel_version}-{git_sha}`.
    The `{role}` segment (control-plane | compute-only) is **removed**.
  - **The image bakes every daemon binary** — all 8 daemons
    (apid, schedd, vmmd, imaged, meterd, builderd,
    gatewayd-internal, gatewayd-public) and every function-runner
    into `/opt/faas/current/bin/`. Daemons NOT in the box's role
    are present on disk but not started.
  - **Per-role surface (99-faas-role.conf drop-ins, per-daemon
    cgroup slices, per-daemon users) is templated at first-boot by
    `gregalectl release install`**, keyed off `FAAS_BOX_ROLE` from
    the cloud-init user-data the operator passes at server-create
    time. The role-overlay build step (`role-overlay.pkr.hcl:78`)
    is retired.
  - **Per-box identity (node_id, region, control_plane.endpoint,
    sealed.env, host.age, rclone.conf, cosign keys, TLS leaves) is
    unchanged from ADR-111** — first-boot, not image. This ADR is
    purely about collapsing the role segment out of the image.

- **Why:** ADR-111's image-tag contract
  `gregale-compute-{role}-{fc_release}-{kernel_version}-{git_sha}`
  embeds role identity into the image binary. That creates
  `gregale-compute-control-plane-...` and
  `gregale-compute-compute-only-...` as two distinct image binaries
  for the same `(fc_release, kernel_version, git_sha)` triple —
  i.e. a per-category image identity analogous to the per-node
  `compute-01-image` / `compute-02-image` / `compute-03-image`
  pathology the principle rejects. The principle is: **the image
  describes WHAT a node is; bootstrap describes WHO a node is and
  WHICH ROLE it serves**. Role is bootstrap, not image. Two
  consequences follow:

  1. **Operator cognitive load collapses.** "Which image do I
     launch?" has one answer per `(fc, kernel, sha)` instead of two.
     The image-build matrix drops by 50%. The per-tag contract is
     reviewable in 5 seconds.
  2. **In-place role mutation becomes possible.** With role as
     first-boot metadata, `gregalectl release install --role
     <new-role>` re-templates drop-ins, re-carves cgroup slices,
     stops no-longer-needed daemons, starts the new role's
     daemons — without rebuilding the image. This is an
     M8-scale-out capability (a compute box being re-purposed
     into the control-plane, or vice versa) we don't have today
     and that M9 live-migration does NOT solve.

  The image is otherwise unchanged from ADR-111: same
  per-daemon SLSA-style build, same supply-chain pins, same
  immutable `/srv/fc/` skeleton, same first-boot runbook chain
  (pki init → host-age init → sign-keys init → node-key init →
  backup init → release install → doctor --deep).

- **Out of scope (explicitly deferred):**
  - **Per-OS image collapse** — Ubuntu 24.04 vs 26.04 are
    different image binaries (the kernel is part of the image).
    A future PR cluster can collapse OS families if we ever
    legitimately support more than one.
  - **Canonical daemon tarball** (the build-economics rewrite
    that decouples daemon compilation from per-cloud build):
    ADR-112 is a prerequisite for that cluster, but the cluster
    itself is its own ADR (proposed ADR-113). Once role is
    generic, building one daemon tarball + one per-cloud overlay
    is straightforward.
  - **Multi-role boxes** (a single box serving both control-plane
    AND compute-only). Subset is strictly one role per box today
    (per ADR-092); multi-role is a separate decision.

- **Per-tag contract evolution:**

  | ADR-111 (PR #929) | ADR-112 (this) |
  |---|---|
  | `gregale-compute-{role}-{fc_release}-{kernel_version}-{git_sha}` | `gregale-compute-{fc_release}-{kernel_version}-{git_sha}` |
  | Two binaries per (fc, kernel, sha) | One binary per (fc, kernel, sha) |
  | `99-faas-role.conf` templated at packer build-time | `99-faas-role.conf` templated at first-boot |
  | Per-daemon cgroup scopes static | Per-daemon cgroup scopes carved on `release install` |
  | `gregalectl release install --git-sha ...` only | `gregalectl release install --git-sha ... --role ...` (role required for first-boot) + `--role` mutation |
  | Re-rolling a box = rebuild the image | Re-rolling a box = re-run `release install` |

- **First-boot wire shape** (what the operator passes at
  `hcloud server create --user-data-from-file`):

  ```yaml
  #cloud-config
  write_files:
    - path: /etc/faas/first-boot.env
      content: |
        FAAS_BOX_ROLE=control-plane
        NODE_ID=fsn-1
        REGION=eu-central
        CONTROL_PLANE_ENDPOINT=https://control.gregale.dev
        GIT_SHA=abc1234d
  runcmd:
    - bash /usr/local/bin/faas-first-boot/runbook-step-4.sh
    ...
    - bash /usr/local/bin/faas-first-boot/runbook-step-10.sh
  ```

  `FAAS_BOX_ROLE` is the only one that flips per role; the others
  flip per node. The image bytes are identical for both.

- **PR cluster:**

  | # | PR | Header | Verification |
  |---|---|---|---|
  | 1 | (this ADR) | ADR-112 lands | Merges to `docs/adr/` |
  | 2 | `#932` (PR-A) | `feat(image): drop {role} from image-tag contract; maximal daemon set in image` | packer-validate green; image builds one binary per (fc, kernel, sha); control-plane.yaml + compute-only.yaml run from same image tag |
  | 3 | `#933` (PR-B) | `feat(release-install): in-place role mutation via --role flag` | e2e: re-role a box control-plane → compute-only → control-plane, sealed-env + host.age + TLS leaves intact |

- **Cross-references:**
  - [[ADR-110 · Declarative split-box deployment manifest]] — the
    role-family split (control-plane vs compute-only) is the same
    unit ADR-112 is talking about; ADR-110 defines the manifest
    schema, ADR-112 refactors WHERE the role family is templated.
  - [[ADR-092 · Gate B cross-box mTLS hardening]] — per-role PKI
    subset must NOT include control-plane daemons on a compute
    image. ADR-112 makes this trivially enforceable (just don't
    start those daemons), and PR-B explicitly verifies the subset.
  - `docs/adr/005-snapshot-pin-fc-version.md` (ADR-005) — the
    per-tag `fc_release` segment stays; it's the
    WHAT-this-box-is content, not the
    WHICH-ROLE-this-box-serves content. ADR-112 does NOT amend
    ADR-005.
  - `docs/adr/110-declarative-split-box-manifest.md` (ADR-110) —
    the manifest's `host.role` field becomes the canonical
    source of `FAAS_BOX_ROLE`; PR-A wires
    `gregalectl release install` to read it.

- **Verification gates (per-PR):**

  PR-A (`#932`):
  ```
  # In a fresh worktree:
  packer validate -syntax-only deploy/packer/common.pkr.hcl
  packer validate -syntax-only deploy/packer/hcloud.pkr.hcl
  packer validate -syntax-only deploy/packer/amazon-ebs.pkr.hcl
  packer validate -syntax-only deploy/packer/iso.pkr.hcl
  # Build a single canary image; both control-plane AND
  # compute-only yaml.tpl files launch from it:
  make image-build-hcloud-canary
  make image-test-first-boot IMAGE_TAG=gregale-compute-fc1.7.0-kernel6.1.134-gcanary123 ROLE=control-plane
  make image-test-first-boot IMAGE_TAG=gregale-compute-fc1.7.0-kernel6.1.134-gcanary123 ROLE=compute-only
  # Both should exit 0; the image bytes are identical.
  ```

  PR-B (`#933`):
  ```
  make image-test-role-mutation \
    IMAGE_TAG=gregale-compute-fc1.7.0-kernel6.1.134-gcanary123 \
    INITIAL=control-plane TARGET=compute-only BACK=control-plane
  # Asserts:
  # - re-role C → CO stops apid / schedd / builderd, starts vmmd
  # - sealed.env + host.age + rclone.conf + TLS leaves unchanged
  # - compute_nodes.role flips via release_bundles row UPSERT
  ```

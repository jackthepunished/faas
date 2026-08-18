# ADR-113 · Canonical daemon tarball (tar + sig + sbom)

- **Status:** **Proposed**
- **Date:** 2026-08-16
- **Decision:** A Gregale release is shipped, verified, and installed as
  a **triple of canonical artifacts**:

  1. `release.tar.gz` — every daemon binary + every function runner +
     the `release-manifest.json` (the existing per-release manifest;
     PRD-3 shape) + `tools/` (helper scripts the daemons exec).
     sha256 is the canonical digest.
  2. `release.cosign.bundle` — cosign signature bundle over the tarball
     (keyless OIDC, Fulcio + Rekor transparency log). Verifies the
     tarball was signed by the CI identity of the `faas` GitHub repo
     at a given commit.
  3. `release.sbom.json` — SPDX-2.3 SBOM (CycloneDX-equivalent is
     rejected — CycloneDX fields the e2e CVE-baseline does not
     consume), embedded as a tarball member so the on-host verifier
     can fetch it without a second HTTP round-trip.

  The host-side `gregalectl release install` flow becomes:

  ```
  pull tarball + sig + sbom by git_sha (operator-provided; resolved
  via `/etc/faas/release-source.conf` for production, OR
  `--tarball-path` for air-gap)
  cosign verify-blob \
      --signature release.cosign.bundle \
      --certificate-identity https://github.com/poyrazK/faas/.github/workflows/build-sha256.yml@refs/tags/<tag> \
      --certificate-identity-regexp "https://github.com/poyrazK/faas" \
      --certificate-oidc-issuer https://token.actions.githubusercontent.com \
      release.tar.gz
  tarball extraction -> per-binary sha256 matched against manifest
  SBOM CVE baseline -> fail-closed on CRITICAL/HIGH regression vs last
                       known-good
  pkg/releaseinstall.AtomicFlip (unchanged)
  ```

  The Packer `image-build-canary` (PR #929, deploy/packer/) **is the
  producer**, not just the consumer. The tarball is built ONCE, in the
  canary's CI runner, after `make build-sha256`. The image bakes the
  tarball itself (so a freshly-installed image can run from local
  artifact if the production source is unreachable), and an OCI
  artifact (`oci://ghcr.io/poyrazk/faas/release@sha256:<digest>`) is
  pushed in parallel as the canonical reference. Day-2 upgrades pull
  from OCI by default; operators can pin `--tarball-path` for air-gap.

  The old `cmd/gregalectl/releaseinstall/copyBinIntoRelease` is
  retired. The new `pkg/releaseinstall/Tarball.{Build,Verify,Extract}`
  owns the producer side; `pkg/releaseinstall/SBOMBaseline` is the
  CVE-baseline diff.

- **Why:** Three reasons, ranked by ship-blocker weight:

  1. **Issue #597 (tier-2 ship-blocker):** "Sign every daemon binary
     with cosign (keyless OIDC) and verify on the host before install."
     Today the host has no way to know whether the daemons it runs
     were built by Gregale CI or by an attacker who SSH'd into the
     Packer runner. The cosign OIDC identity is the verifiable bit.
  2. **Issue #601 (security, needs-ADR):** "Add CVE-vs-SBOM diff job
     in CI; alert when a new vuln affects a runtime dep." Today
     nothing knows which CVEs a given Gregale release ships with.
     SBoM embedded in the tarball closes the audit gap AND enables
     a host-side regression gate that does not need internet.
  3. **Operational:** `copyBinIntoRelease` is a host-side walk of a
     bin-dir the operator hands to the bundle subcommand. It is the
     last unscheduled day-1 piece that still trusts the operator's
     filesystem. PR-B (#932) shipped the day-2 re-role path; day-1
     install deserves the same trust level.

  Alternative considered and rejected:
  **"Single tarball, no cosign, no SBoM"** — closes the
  `copyBinIntoRelease` ergonomics gap but leaves issues #597 and #601
  open. The cluster is small enough (one producer + one verifier) that
  doing it once with all three is cheaper than doing it twice. PR-A
  ships the tarball + cosign + SBoM together.

- **Consequences:**
  - **Packer `image-build-canary` must learn to publish.**
    `deploy/packer/image-build-canary.pkr.hcl` runs the canary build,
    then a `post-process` step pushes the tarball to
    `oci://ghcr.io/poyrazk/faas/release` (skipped on local dev;
    gated on `CI=true`).
  - **`pkg/releaseinstall` gains three types.**
    `Tarball{Spec, Build, Verify, Extract}`, `CosignVerifier`
    (interface; production impl wraps `cosign verify-blob` via exec,
    test impl is a fixture), and `SBOMBaseline{Diff,
    KnownGood}`.
  - **Host-side crypto:**
    The host MUST trust Fulcio root CA + Rekor public key.
    `/etc/faas/release-source.conf` (new file, owned by ADR-111
    first-boot / pkg/releaseinstall.Configure) carries the issuer
    and identity-regex the operator pins. Out-of-scope-by-default
    means a fresh box that has never been initialized refuses to
    install until the config is filled in.
  - **Doctor (`gregalectl doctor --deep`) gets a fourth probe.**
    `Verify` walks the on-disk tree, looks up the corresponding
    SBoM, and asserts the SBOM's critical/high CVE count has not
    regressed vs the KGV (Known Good Version) recorded at last
    install. Drift surfaces as `severity=warn`.
  - **Build-time fail-closed:**
    `make build-sha256` MUST `cosign sign-blob` + `syft` SBoM-emit
    after the daemon binaries are built. If either fails, the build
    fails; `release bundle` is not allowed to ship a partial triple.
  - **Backwards compat:**
    PRD-3 release bundles stay readable: a `--legacy-bundle-dir` flag
    on `release install` consumes the old `git_sha/` directory shape
    via `copyBinIntoRelease` (the function stays but is gated to that
    code path). It is the sunset path; no new operator docs reference
    it.

- **Cluster table (PR-A + PR-B):**

  | PR | Title | Atomic commits |
  |---|---|---|
  | PR-A #XXX | canonical daemon tarball (build + verify) | (1) Tarball producer — Packer + `pkg/releaseinstall/Tarball`.<br>(2) host-side `cosign verify-blob` (issue #597 partial).<br>(3) SBOM embed + CVE-baseline gate (issue #601 partial). |
  | PR-B #YYY | tarball-only day-2 + KGV rotation | (1) `release install` ON-FAIL re-reads tarball by digest from `/etc/faas/release-source.conf` (instead of cloning per-tag).<br>(2) `KGV rotate --prev-sha SHA` subcommand for `gregalectl release`; writes the SBOM critical/high baseline.<br>(3) `doctor --deep` new probe. |

  PR-A ships the producer + consumer of the triple. PR-B ships the
  day-2 ergonomics + the doctor probe. Both PRs together close #597
  and #601.

- **Does NOT change (load-bearing, per CLAUDE.md):**
  - `schedd` remains the only writer to `instances` (§6).
  - The release-manifest content shape (`pkg/releaseinstall.Manifest`)
    does not change; it just becomes a tarball member.
  - Image-tag contract stays
    `gregale-compute-{fc_release}-{kernel_version}-{git_sha}` per
    ADR-112. ADR-113 does not re-introduce the role segment.
  - `copyBinIntoRelease` stays available behind `--legacy-bundle-dir`
    for the sunset window.

- **Cross-PR dependencies:**
  - **ADR-111 (Gregale Compute Image) — merged.** Provides the
    Packer runner ADR-113 publishes from.
  - **PR #929 (image-build-canary + deploy/packer) — merged.**
    Provides `deploy/packer/image-build-canary.pkr.hcl`. ADR-113
    adds a `post-process` step to that runner.
  - **PR-A #930 (ADR-112 role-image-collapse) — merged.** Tags the
    image without role segment; the tarball is built off the same
    un-tagged `git_sha`.
  - **PR-A fixes #931 — merged.** Provides the daemonInfoTable
    ADR-113's verifier cross-checks.
  - **PR-B #932 (in-place role mutation) — merged.** The day-2
    hook the tarball feeds. Without PR-B, `release install --role X`
    wouldn't need the tarball; with PR-B, every re-role is a
    tarball re-pull.
  - **PR #924 cluster — merged.** Multi-host plumbing
    (`HostBridgeCIDR`, `MasqueradeCIDR`) the day-3 tarball-pull
    reuses for cross-node artifact distribution.

- **Verification gates (per-PR):**

  **PR-A:**
  - [ ] `make build-sha256` produces `release.tar.gz`,
        `release.cosign.bundle`, `release.sbom.json`; the
        `cosign verify-blob` round-trip passes against the CI OIDC
        issuer.
  - [ ] `pkg/releaseinstall/Tarball_Build_HashStable` —
        byte-identical tarball across rebuilds (modulo mtimes
        normalized).
  - [ ] `pkg/releaseinstall/Cosign_Verify_RejectsTampered`
        (negative case: flip a bit in the tarball, expect
        verification failure).
  - [ ] `pkg/releaseinstall/SBOM_DiffDetectsCriticalRegression`
        (add a critical CVE to the SBOM, expect regression).
  - [ ] `cmd/e2e/image_tarball_install_test.go` (e2eimage):
        spawn VM, `gregalectl release install --git-sha HEAD`
        succeeds; second install with `--tarball-path Pinned_OLD`
        fails-closed with the regression message.
  - [ ] `make lint build` green.

  **PR-B:**
  - [ ] `cmd/gregalectl/commands_release.go` gains
        `subReleaseKGV` (rotate | show) with manifest entry.
  - [ ] `pkg/doctor` gains `VerifyTarballSBOM` ProbeTarget; CI
        keeps it green across a hand-crafted regression.
  - [ ] e2e: `KGV rotate`, then add a critical CVE in
        a simulated SBoM patch, then `doctor --deep` exits non-zero
        with the diff visible.

- **How to read this ADR next review:**
  - **Cluster table for ADR-113 (PR-A + PR-B)** is the load-bearing
    section. The split between PR-A (producer + first verifier) and
    PR-B (day-2 KGV + doctor probe) is deliberate: each cell is
    reviewable in ≤10 min.
  - **Cross-PR dependencies** tells you what to read first. If
    you're new to the cluster, read ADR-112 → PR-A #930 → PR-B #932
    first; ADR-113 builds on them.
  - **Does NOT change** is the section the load-bearing preservation
    reviewers will audit (per CLAUDE.md's "Things that look wrong but
    are load-bearing" list).

  See also: [[multi-host-scaleout-gap]] — Day-3 cross-node tarball
  replication is the next cluster after this ADR closes.

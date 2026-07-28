# ADR-040 — OCI layer symlink policy: store verbatim, clamp on traversal

Status: Accepted, 2026-07-28. Owner: @poyrazK. Supersedes the symlink
half of commit 7805f76. Related: spec §4.6 (two-drive rootfs), §9.1
(build-input attack surface), ADR-038 (build attestation).

## Context

`pkg/rootfs.ApplyLayer` unpacks OCI/Docker layers into a staging tree
that `mkfs.ext4 -d` turns into drive0/drive1. Layer bytes are
attacker-controlled: a customer's Dockerfile decides what goes in the
tar, so path traversal is a real attack surface (§9.1).

CodeQL's `go/path-injection` rule flagged the `os.Symlink(hdr.Linkname,
target)` call. Commit 7805f76 silenced it by routing `hdr.Linkname`
through `safeJoin`, which rejects absolute paths and `..`.

That fix was wrong in two ways, and the second one broke production:

1. **It corrupted relative links.** `safeJoin` returns
   `filepath.Join(base, linkname)`, so a link declared as `sibling` was
   written to disk as `/tmp/faas-base-XXXX/sibling` — the host staging
   path, baked into the image. Every such symlink dangles once the ext4
   is mounted inside the guest.

2. **It rejected absolute link targets outright**, and those are the
   norm, not an attack. The alpine base image ships 306 of them (every
   `bin/*` applet → `/bin/busybox`); Debian and Ubuntu do the same.
   `imaged` therefore could not build `builder-base.ext4` from any real
   OCI image. It exited 1 at startup in a `Restart=` loop — 18,991
   restarts — and took `cd-digitalocean` red on every merge to main:

   ```
   imaged: stage builder base docker.io/library/alpine@sha256:79ff19… →
     /srv/fc/base/builder-base.ext4: imaged: build base ext4:
     rootfs: apply base layer 0:
     rootfs: absolute entry path "/bin/busybox" rejected
   ```

The underlying category error: treating a symlink's `Linkname` as a
*host* path. It is not. It is guest-side data — an opaque string stored
in the ext4 inode and resolved by the guest kernel at use time, where
the staging root *is* `/`.

## Decision

**1. Symlink `Linkname` is stored verbatim.** Absolute stays absolute,
relative stays relative. No host-side rewriting, no rejection. This is
the only behaviour that produces a correct rootfs, and it is what every
other unpacker (containerd, moby, podman) does.

**2. The security boundary moves to the write path.** The real risk was
never the link's text — it is *following* a hostile link during unpack
("`bin` → `/etc`", then write `bin/passwd`). `resolveWithin` walks each
entry's ancestor components and **clamps** symlinks inside the staging
root, chroot-style: an absolute link target re-anchors at the staging
root, and `..` at the root stays at the root.

**3. Clamp, do not reject.** Rejecting on an ancestor symlink would be
simpler but breaks legitimate images: Debian's merged-usr layout
(`bin` → `usr/bin`) depends on exactly this traversal. Clamping is
strictly safer than the pre-7805f76 code *and* compatible with every
real image, so there is no compatibility/security trade to make.

**4. The final path component is not resolved.** An OCI entry for path
X replaces whatever sits at X (overlayfs semantics), so a regular-file
entry landing on an existing symlink clobbers the link rather than
writing through it.

**5. Entry names keep the strict syntactic gate.** `safeJoin` still
rejects absolute and `..`-containing `hdr.Name` values — real images
never produce them, so strictness costs nothing there. Hardlink
`Linkname`s do resolve host-side (`os.Link` needs a real path) and keep
the same gate, with a leading `/` stripped per tar semantics.

Symlink chasing is bounded at 40 hops (`maxSymlinkHops`, mirroring the
kernel's `SYMLOOP_MAX`) so a link cycle fails the build instead of
hanging the daemon.

## Consequences

- `imaged` can build a base ext4 from any standard OCI image again.
- The `go/path-injection` alert is answered at the sink that actually
  matters. **If CodeQL flags `os.Symlink(hdr.Linkname, …)` again, the
  correct response is to confirm the clamp still holds — not to
  sanitise `Linkname`.** `TestApplyLayer_WriteThroughSymlinkIsClamped`
  and `TestApplyEntry_Symlink_TwoStepChainCannotEscapeWrites` exist to
  make that argument concrete; `TestBuildBase_RealImageShapeWithAbsoluteSymlinks`
  fails the build if anyone reverts to rejection.
- `resolveWithin` is a non-atomic lstat walk. Sound here because the
  staging tree is a fresh `os.MkdirTemp` written by a single goroutine
  of one daemon; the attacker controls tar bytes, not concurrent
  filesystem access. **If staging ever becomes shared or concurrently
  written, this must move to `openat2(RESOLVE_IN_ROOT)`.**
- Cost is one `lstat` per path component per entry — negligible against
  the `mkfs` that follows.

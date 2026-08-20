# builder-base — rootfs for the ephemeral builder microVM (spec §4.5, ADR-003).
# Contains BuildKit, Railpack, git, and the OCI exporter. Builds run INSIDE this
# VM so untrusted `npm install` gets VM-grade isolation; the 2 GB RAM cap is the
# VM boundary. Never run builds in a host container.
#
# Multi-arch: TARGETARCH is set automatically by `docker buildx build`. The
# EX44 builds --platform=linux/amd64 (production target). The Lima dev loop
# builds --platform=linux/arm64 so the local metal-lima path can exercise a
# real builder VM end-to-end. The arm64 build does NOT replace the §14 M6
# acceptance gates — those still need the EX44 — but it does prove the
# spawn → runBuild → DestroyWithExport path works against a real artifact
# producing engine rather than a busybox stub.
#
# Both railpack and buildkit are pulled as upstream release tarballs (neither is
# packaged in Alpine). Versions are pinned via build-args so CI can
# override them per release without churning this file.

# ---- railpack (Node/Python builder, spec §4.5) ---------------------------
# Upstream switched from flat `-linux-amd64` binaries to Rust-target-triple
# names in v0.10+. The current naming is `-x86_64-unknown-linux-musl` /
# `-arm64-unknown-linux-musl`. v0.5.0 with the old naming is no longer
# published, so bumping to v0.31.1 (current stable as of 2026-07) is mandatory.
ARG RAILPACK_VERSION=0.31.1

# Railpack 0.31.1 bootstraps mise 2026.7.6 using its glibc linux-x64
# asset. The builder rootfs is Alpine, so stage the matching musl asset and
# let guest-init seed Railpack's expected cache path before prepare runs.
ARG MISE_VERSION=2026.7.6

# ---- buildkit (Dockerfile builds, spec §4.5 fallback path) ----------------
# Rootless inside the VM — rootless-runc inside a VM is functionally root, and
# the VM boundary is the actual security perimeter (ADR-003).
ARG BUILDKIT_VERSION=0.31.2

# ---- guest-init version (issue #938 / PR-B / ADR-114) -------------------
# Multi-arch builds CANNOT pre-stage guest-init in the build context because
# both arches would overwrite the same host path (review finding #2 on PR
# #940). Instead, build guest-init inside the Dockerfile via cross-compile
# so each arch's binary lands in its own image. The Go builder base is
# digest-pinned via images/Dockerfile.lock just like alpine:3.22 below.
# Issue #938: building guest-init inside the image (instead of in the
# workflow) also lets the Lima local-build path stage a multi-arch rootfs
# via buildx without per-arch file juggling.
# Note: the version is intentionally baked into the FROM line (no ARG)
# so images/Dockerfile.lock has a literal "golang:1.25.9" alias to
# match against. Bumping the Go version is a two-step: change this
# line, run `make images-lock-update` to refresh the lock and digest.
# We use 1.25.9 (not 1.23.x) because BuildKit v0.31.2 requires
# Go 1.25.9 and the repo's `tool` directive rejects older Go versions
# with `unknown directive: tool` (verified during PR #940 review).

# ---- stage 1: build guest-init for the target arch -----------------------
# Image registry digest pinned via images/Dockerfile.lock; make
# images-lock-update resolves the current digest and rewrites BOTH
# this line and the lock entry. The base manifest-list digest pins
# every per-arch child manifest so buildx's per-arch resolution
# stays race-free under multi-arch build (per-arch digests are not
# stable across re-pulls, but the manifest-list digest is).
# $TARGETPLATFORM is implicit on multi-arch FROM; the explicit
# `--platform=` would emit a RedundantTargetPlatform warning.
FROM golang:1.25.9@sha256:8a7adc288b77e9b787cd2695029eb54d10ae80571b21d44fed68d067ad0a9c96 AS guest-init-build
WORKDIR /src
# guest-init is a pure-Go binary; no submodule vendoring needed. The
# repository is the build context, so COPY . picks up the whole tree.
# .dockerignore (repo root) keeps secrets, .git, and local caches out.
COPY . /src
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
      go build -trimpath -tags linux \
        -o /out/faas-guest-init ./guest/init

# BuildKit's server has a deliberately strict session liveness check. The
# stock buildctl release has no flag for its per-session timeout header, while
# slow bare-metal builders can spend several minutes importing a remote layer.
# Build the tiny upstream client with the repository patch that opts into a
# bounded, longer session interval; buildkitd itself remains the pinned
# upstream release binary below.
FROM golang:1.25.9@sha256:8a7adc288b77e9b787cd2695029eb54d10ae80571b21d44fed68d067ad0a9c96 AS buildkit-client-build
WORKDIR /src/buildkit
ARG BUILDKIT_VERSION
ARG TARGETOS
ARG TARGETARCH
COPY images/buildkit-session-health.patch /tmp/buildkit-session-health.patch
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl git && \
      rm -rf /var/lib/apt/lists/* && \
      curl -fsSL "https://github.com/moby/buildkit/archive/refs/tags/v${BUILDKIT_VERSION}.tar.gz" | \
        tar -xzf - --strip-components=1 -C /src/buildkit && \
      git apply /tmp/buildkit-session-health.patch && \
      CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
        go build -trimpath -o /out/buildctl ./cmd/buildctl

# ---- stage 2: assemble the runtime rootfs -------------------------------
# See the stage 1 FROM above re: $TARGETPLATFORM handling.
# Docker's build-time /etc/resolv.conf is a read-only injected mount, so keep
# the guest resolver as a real file in a scratch stage and COPY it into the
# final filesystem. The guest-init fallback remains for operator overrides.
FROM scratch AS builder-resolver
COPY images/builder-resolv.conf /etc/resolv.conf

# Alpine supplies the small, currently supported userland for the builder
# VM. The image reference is digest-pinned via images/Dockerfile.lock.
FROM alpine:3.22@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce
# Issue #197 B3.5: the `alpine:3.22` tag is mutable. The digest is
# pinned via images/Dockerfile.lock; `make images-lock-update` resolves
# the current registry digest and updates BOTH the lock and the FROM
# line. CI runs `make images-lock-check` to fail any PR that drifts.
ARG RAILPACK_VERSION
ARG BUILDKIT_VERSION
ARG MISE_VERSION
ARG TARGETARCH

RUN apk add --no-cache \
      git ca-certificates curl xz shadow-subids fuse-overlayfs runc util-linux util-linux-misc

# guest-init and BuildKit use the stable platform path for the OCI runtime;
# Alpine packages runc under /usr/bin.
RUN ln -s /usr/bin/runc /usr/local/bin/runc

# guest-init uses util-linux unshare's automatic subordinate-ID mapping. The
# BusyBox applet accepts neither --map-users nor --map-groups, so assert the
# actual runtime contract while assembling the image instead of discovering a
# stale/incomplete builder rootfs only after a VM has booted.
RUN test -x /usr/local/bin/runc && \
    test -x /usr/bin/unshare && \
    /usr/bin/unshare --help 2>&1 | grep -q -- '--map-users'

# Rootless BuildKit runs inside the builder microVM's user namespace. Give
# the mapped root a bounded subordinate range so runc can materialise image
# ownership (for example root:shadow) without falling back to host access.
RUN printf 'root:100000:65536\n' > /etc/subuid && \
    printf 'root:100000:65536\n' > /etc/subgid

# BuildKit rootless. Two files: buildkitd (daemon) + buildctl (client). The
# upstream tarball unpacks both into ./bin/.
RUN mkdir -p /opt/buildkit && \
      curl -fsSL -o /tmp/buildkit.tgz \
      "https://github.com/moby/buildkit/releases/download/v${BUILDKIT_VERSION}/buildkit-v${BUILDKIT_VERSION}.linux-${TARGETARCH}.tar.gz" && \
      tar -C /opt/buildkit -xzf /tmp/buildkit.tgz && \
      rm /tmp/buildkit.tgz && \
      install -m 0755 /opt/buildkit/bin/buildkitd /usr/local/bin/buildkitd && \
      rm -rf /opt/buildkit

COPY --from=buildkit-client-build /out/buildctl /usr/local/bin/buildctl
RUN chmod 0755 /usr/local/bin/buildctl

# Railpack. The current naming convention is `<ver>-<arch>-unknown-linux-musl.tar.gz`
# where <arch> is `x86_64` or `arm64`. We resolve the right arch from TARGETARCH.
RUN case "${TARGETARCH}" in \
      amd64) RAILPACK_ARCH=x86_64 ;; \
      arm64) RAILPACK_ARCH=arm64 ;; \
      *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac && \
    curl -fsSL -o /tmp/railpack.tgz \
      "https://github.com/railwayapp/railpack/releases/download/v${RAILPACK_VERSION}/railpack-v${RAILPACK_VERSION}-${RAILPACK_ARCH}-unknown-linux-musl.tar.gz" && \
    tar -C /usr/local/bin -xzf /tmp/railpack.tgz railpack && \
      chmod +x /usr/local/bin/railpack && \
      rm /tmp/railpack.tgz && \
      /usr/local/bin/railpack --version

# Railpack currently downloads a glibc mise asset at build time. Keep a
# musl-compatible copy in the builder image; guest-init stages it into the
# tmpfs-backed Railpack cache immediately before the build starts.
RUN case "${TARGETARCH}" in \
      amd64) MISE_ARCH=x64 ;; \
      arm64) MISE_ARCH=arm64 ;; \
      *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac && \
    mkdir -p /usr/local/lib/faas/mise /opt/mise && \
    curl -fsSL -o /tmp/mise.tgz \
      "https://github.com/jdx/mise/releases/download/v${MISE_VERSION}/mise-v${MISE_VERSION}-linux-${MISE_ARCH}-musl.tar.gz" && \
    tar -C /opt/mise -xzf /tmp/mise.tgz && \
    install -m 0755 /opt/mise/mise/bin/mise \
      "/usr/local/lib/faas/mise/mise-${MISE_VERSION}" && \
    rm -rf /opt/mise /tmp/mise.tgz

# curl is only an image-build transport; the guest uses BuildKit, Railpack,
# git, and fuse-overlayfs at runtime. Removing curl (and its orphaned
# libcurl dependency) keeps the builder admission scan free of transport
# vulnerabilities that cannot be fixed by the current Alpine repository.
RUN apk del curl

# guest-init copied from the build stage. Each arch's manifest receives the
# arch-matching binary because buildx resolves TARGETARCH per image in the
# multi-arch build — no host-side pre-build required, no overwrite bug.
COPY --from=builder-resolver /etc/resolv.conf /etc/resolv.conf
COPY --from=guest-init-build /out/faas-guest-init /usr/local/bin/faas-guest-init
RUN chmod +x /usr/local/bin/faas-guest-init

WORKDIR /build

# BuildKit is deliberately launched rootless inside the builder VM's user
# namespace. The workspace is disposable VM-local state, so it must be
# writable by the mapped worker uid rather than relying on the image's root
# ownership surviving OCI-to-ext4 materialisation.
RUN chmod 0777 /build

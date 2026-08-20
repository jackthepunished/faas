# runner-node22 — base rootfs (drive0) for Node 22 apps and functions (spec §4.6,
# §4.9). The base is content-addressed and staged once as drive0; app images
# still contribute only their dependency/code delta as drive1.
# Content-addressed, staged to /srv/fc/base/runner-node22.ext4.
FROM node:22-alpine@sha256:76789712cd1ae89a1225eac9077010d68987a423588042dac30446f502f1858c
# Issue #197 B3.6: mutable tag pinned via images/Dockerfile.lock.
# `make images-lock-update` is the only way to bump the digest.
# This runtime is intentionally self-contained rather than composed over the
# shared Debian parent: musl and glibc are not interchangeable, and applying
# Debian parent layers would produce an image that scans cleanly but cannot
# boot reliably.
# Guest runtime user (uid 1000, spec §4.8).
RUN id app 2>/dev/null || adduser -D -u 1000 app
# The function runner shim (guest/runners/node22) is layered in for `type:
# function` deploys at M7; plain Node apps bring their own entrypoint.
WORKDIR /app

# runner-go124 — base rootfs (drive0) for Go 1.24 apps and functions (spec §4.6,
# §4.9). Built FROM the official golang:1.24-bookworm image so glibc matches
# the toolchain Railpack uses to emit the customer's static binary. The
# per-app layer (drive1) carries the customer's compiled handler at
# /app/handler (function mode) or /app/server (app mode); the base only
# needs the runtime + standard library.
#
# This base is the largest of the three runtimes (~350 MB uncompressed).
# The two-drive scheme amortizes it across every go124 app on the box —
# per-app cost is just the static binary (5-30 MB) — so the 130 MB/sandbox
# accounting is preserved (CLAUDE.md "load-bearing — DO NOT fix").
#
# The image is built and published by CI. imaged resolves the immutable
# per-runtime reference and auto-stages the matching ext4 on first use, so a
# newly provisioned compute node does not need a hand-copied runtime image.
FROM golang:1.24-bookworm@sha256:98d673f18a1aac43da744209873cb79323e11706f909251bcfb131828b95559d
# Issue #197 B3.6 (extension): mutable tag pinned via images/Dockerfile.lock.

# Guest runtime user (uid 1000, spec §4.8).
RUN id app 2>/dev/null || useradd -u 1000 -m app

# The function runner shim (guest/runners/go124) is layered in for
# `type: function` deploys; plain Go apps bring their own entrypoint
# (the customer-emitted static binary at /app/server).
WORKDIR /app

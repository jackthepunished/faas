# runner-node24 — base rootfs (drive0) for Node 24 LTS function
# deploys (spec §4.6, §4.9). Pairs with guest/runners/node24/main.go.
# Content-addressed, staged to /srv/fc/base/runner-node24.ext4.
#
# Tier 1 PR 2 (ADR-052): this base is now AUTO-STAGED by imaged at
# startup via pkg/imaged/base_stage.go::EnsureBases, mirroring the
# builder-base path. The operator recipe below remains valid for
# boxes that haven't upgraded imaged yet (fallback staging).
#
# The two-drive scheme amortizes this base across every node24 app on
# the box — per-app cost is just the customer's package.json-resolved
# node_modules + handler. The 130 MB/sandbox accounting is preserved
# (CLAUDE.md "load-bearing — DO NOT fix").
FROM node:24-bookworm-slim@sha256:REPLACE_ME_AT_MERGE_TIME
# Issue #197 B3.6: mutable tag pinned via images/Dockerfile.lock.
RUN id app 2>/dev/null || useradd -u 1000 -m app
WORKDIR /app

# runner-python313 — base rootfs (drive0) for Python 3.13 function
# deploys (spec §4.6, §4.9). Pairs with guest/runners/python313/main.go.
# Content-addressed, staged to /srv/fc/base/runner-python313.ext4.
#
# Tier 1 PR 2 (ADR-052): this base is now AUTO-STAGED by imaged at
# startup via pkg/imaged/base_stage.go::EnsureBases, mirroring the
# builder-base path. The operator recipe below remains valid for
# boxes that haven't upgraded imaged yet (fallback staging).
#
# The two-drive scheme amortizes this base across every python313 app
# on the box — per-app cost is just the customer's site-packages +
# handler. The 130 MB/sandbox accounting is preserved
# (CLAUDE.md "load-bearing — DO NOT fix").
FROM python:3.13-slim-bookworm@sha256:0f16c5d35fe6464ee471792ab3bb9116f911b65b3fbf10120c98d2bdc6332f48
# Issue #197 B3.6: mutable tag pinned via images/Dockerfile.lock.
RUN id app 2>/dev/null || useradd -u 1000 -m app
WORKDIR /app

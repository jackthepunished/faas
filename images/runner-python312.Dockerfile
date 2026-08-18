# runner-python312 — base rootfs (drive0) for Python 3.12 apps and functions
# (spec §4.6, §4.9). Same two-drive rationale as runner-node22.
# Content-addressed, staged to /srv/fc/base/runner-python312.ext4.
FROM python:3.12-slim-bookworm@sha256:a116514e19457bcb7af7efe9c3dd0b9b71e85b317694e7882a1c52aa15a78134
# Issue #197 B3.6: mutable tag pinned via images/Dockerfile.lock.
RUN id app 2>/dev/null || useradd -u 1000 -m app
WORKDIR /app

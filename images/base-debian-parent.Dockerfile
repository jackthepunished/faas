# base-debian-parent — shared staging-only parent for the Debian-backed
# runtimes (ADR-053). Keep this Dockerfile as a direct FROM so its first
# layer is byte-identical to the Debian layer used by node24/python312/
# python313. imaged composes those children by matching OCI diff IDs.
#
# Node22 is intentionally Alpine and does not use this parent.
FROM debian:12-slim@sha256:362e64223cc0da95422b3b13c045186fc0a81250e765d31c025fbddf257f6143

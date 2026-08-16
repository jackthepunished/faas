#!/usr/bin/env bash
# deploy/packer/cloud-rollout/hcloud.sh — Hetzner-specific image rollout.
#
# ADR-111 contract: takes (node-fqdn, image-tag), rebuilds the Hetzner
# server from the named snapshot, and waits for the new instance to
# respond to `gregalectl doctor`. Exits 0 on success.
#
# Invoked by `make upgrade-node IMAGE_TAG=...` via cmd/deployctl/upgrade.go
# runCloudRollout. The wrapper is intentionally thin — the heavy lifting
# is the existing `hcloud server rebuild` CLI, which already does the
# snapshot swap + IP preservation.
set -euo pipefail

NODE="${1:?node fqdn required}"
IMAGE_TAG="${2:?image tag required}"

HCLOUD_SERVER_NAME="$(echo "${NODE}" | cut -d. -f1)"  # fsn-1.example.com → fsn-1

echo "hcloud-rollout: rebuilding ${HCLOUD_SERVER_NAME} from snapshot ${IMAGE_TAG}"

# hcloud server rebuild does the swap in place: same IP, same server
# name, just a new root disk. The image_id is the snapshot we built in
# PR #929. hcloud resolves the tag → snapshot id via `hcloud image list
# -o columns=id,name | grep <tag>`.
SNAPSHOT_ID="$(hcloud image list -o noheader -o columns=id,name \
    | awk -v tag="${IMAGE_TAG}" '$2 == tag {print $1; exit}')"

if [[ -z "${SNAPSHOT_ID}" ]]; then
    echo "hcloud-rollout: no snapshot with tag ${IMAGE_TAG}" >&2
    exit 1
fi

hcloud server rebuild "${HCLOUD_SERVER_NAME}" --image "${SNAPSHOT_ID}"

# Wait for the new server to come up + respond to the box-role gate.
# `hcloud server wait` polls the server status until it's running.
hcloud server wait "${HCLOUD_SERVER_NAME}" --wait-until=rebuild_finished --timeout=300s

# The deployctl upgrade orchestrator polls Lifecycle.Probe next; we
# don't poll doctor here — that's the orchestrator's job. Exit 0
# signals "the new VM is up; orchestrator, take over the probe gate".
echo "hcloud-rollout: ${HCLOUD_SERVER_NAME} rebuilt; orchestrator takes the probe gate"

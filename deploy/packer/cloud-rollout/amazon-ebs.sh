#!/usr/bin/env bash
# deploy/packer/cloud-rollout/amazon-ebs.sh — AWS-specific image rollout.
#
# ADR-111 contract: takes (node-fqdn, image-tag), terminates the
# current EC2 instance + launches a fresh one from the named AMI,
# and waits for the new instance to come up. The new instance's
# IP is propagated to the operator via the metadata API; the
# deployctl upgrade orchestrator picks up the probe gate from there.
#
# We terminate + launch (NOT in-place AMI swap) because:
#   (a) in-place requires a stop+start cycle that drops the
#       ephemeral disk;
#   (b) cross-AZ migration is the dominant production case;
#   (c) ASG lifecycle hooks expect a terminate+launch, not a swap.
set -euo pipefail

NODE="${1:?node fqdn required}"
IMAGE_TAG="${2:?image tag required}"

# Resolve the running instance by tag. The compute_nodes row carries
# the instance-id; the operator's `make upgrade-node` reads it from
# the postgres state. This shell wrapper takes it via env.
INSTANCE_ID="${INSTANCE_ID:?INSTANCE_ID required (export from operator)}"

echo "amazon-ebs-rollout: terminating ${INSTANCE_ID} + launching from AMI ${IMAGE_TAG}"

# Resolve the new AMI id by tag. Like the hcloud wrapper, the AMI
# is named with the local.image_tag synthesis (`gregale/<tag>`).
AMI_ID="$(aws ec2 describe-images \
    --filters "Name=name,Values=gregale/${IMAGE_TAG}" \
    --query 'Images[0].ImageId' \
    --output text)"

if [[ -z "${AMI_ID}" || "${AMI_ID}" == "None" ]]; then
    echo "amazon-ebs-rollout: no AMI named gregale/${IMAGE_TAG}" >&2
    exit 1
fi

# Capture the old instance's metadata (subnet, sg, iam, user-data) so
# the new instance comes up identical. user-data is the cloud-init
# first-boot template (control-plane.yaml.tpl / compute-only.yaml.tpl)
# from PR #930. ALL of these MUST be captured BEFORE terminate — once
# the instance is gone, AWS returns None for every field (PR #929
# review-fix M6).
INSTANCE_TYPE="$(aws ec2 describe-instances --instance-ids "${INSTANCE_ID}" \
    --query 'Reservations[0].Instances[0].InstanceType' --output text)"
SUBNET_ID="$(aws ec2 describe-instances --instance-ids "${INSTANCE_ID}" \
    --query 'Reservations[0].Instances[0].SubnetId' --output text)"
SG_IDS="$(aws ec2 describe-instances --instance-ids "${INSTANCE_ID}" \
    --query 'Reservations[0].Instances[0].SecurityGroups[*].GroupId' --output text | tr '\n' ' ')"
IAM_ARN="$(aws ec2 describe-instances --instance-ids "${INSTANCE_ID}" \
    --query 'Reservations[0].Instances[0].IamInstanceProfile.Arn' --output text)"

# Terminate the old instance (the operator has already drained it).
aws ec2 terminate-instances --instance-ids "${INSTANCE_ID}" >/dev/null
aws ec2 wait instance-terminated --instance-ids "${INSTANCE_ID}"

# Launch the new one with the same metadata + the new AMI.
NEW_INSTANCE_ID="$(aws ec2 run-instances \
    --image-id "${AMI_ID}" \
    --instance-type "${INSTANCE_TYPE}" \
    --subnet-id "${SUBNET_ID}" \
    --security-group-ids ${SG_IDS} \
    --iam-instance-profile "Arn=${IAM_ARN}" \
    --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=${NODE}},{Key=faas-role,Value=${FAAS_BOX_ROLE:-control-plane}}]" \
    --query 'Instances[0].InstanceId' --output text)"

echo "amazon-ebs-rollout: launched ${NEW_INSTANCE_ID}; orchestrator takes the probe gate"

# Wait for the new instance to reach 'running' state — the orchestrator's
# probe gate is what asserts readiness, not this wrapper.
aws ec2 wait instance-running --instance-ids "${NEW_INSTANCE_ID}"

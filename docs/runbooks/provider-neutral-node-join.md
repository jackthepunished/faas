# Provider-neutral compute-node join

`gregalectl deploy join-node` adopts an already-created Linux machine into a
manifest-declared `compute-only` fleet. It does not create VMs, call a cloud
API, edit the repository, or require a provider-specific deployment module.

The provider handoff is deliberately small:

1. Create the machine manually at GCP, Hetzner, OVH, or another bare-metal
   provider.
2. Give the operator an SSH address, user, port, and key that can reach it.
3. Ensure the machine can reach the existing fleet over the private transport
   names declared by the manifest.
4. Run the join command from a runner that can reach the current fleet. The
   existing control-plane host is the simplest runner when the operator laptop
   cannot resolve the private names.

Everything after that boundary is owned by the pipeline.

## Lifecycle

The command performs these phases in order:

1. Validate the manifest and require the requested host to be
   `compute-only`.
2. Generate an ephemeral Ansible inventory from the manifest. The generated
   files are removed at the end and never committed.
3. Override only the new host's Ansible connection target with `--ssh-host`.
   Runtime daemon endpoints remain the stable manifest names.
4. Run the complete-fleet Ansible preflight, unless
   `--skip-fleet-preflight` is explicitly supplied after a recent successful
   preflight.
5. Stage the root-only database environment, fleet PKI, image-signing keys,
   signed release assets, bootstrap `gregalectl`, and the manifest.
6. Run the production `deploy/ansible/bootstrap.yml` compute role.
7. Install the signed release with `--defer-activation`; the database row is
   kept drained while the box is being prepared.
8. Render the manifest, initialize host-local identity, start the four
   compute services, and wait for vmmd's socket, the internal gateway, and
   systemd-active status.
9. Activate the compute row only after all readiness gates pass.

If a phase fails, the node remains non-schedulable. Re-running the same command
is the recovery path after correcting the failed input or host condition.

## Required inputs

The release workflow must provide these local artifacts:

- `release.tar.gz`, alongside `release.cosign.bundle` and `release.sbom.json`;
- a Linux/amd64 bootstrap `gregalectl` binary;
- the `cosign` verifier binary;
- a fleet PKI directory containing `ca/ca.crt`, `ca/ca.key`, and the
  compute-only leaves;
- the image-signing private/public key pair;
- a root-only `compute-db.env` containing `DATABASE_URL=...`.

The PKI and signing files are sensitive deployment material. Keep them in the
operator's secret store and use a short-lived workspace for the command. The
pipeline stages them over Ansible with restrictive modes; it does not print
their contents.

Overlay-specific credentials remain Ansible inputs. For example, a Tailscale
fleet supplies its vaulted/authenticated overlay variables with
`--ansible-vars-file`; a static private network needs no extra overlay secret.

## Example

Run a plan first:

```text
gregalectl deploy join-node \
  --manifest-file deploy/manifest/production/gcp-live.yaml \
  --node fsn-3 \
  --ssh-host 203.0.113.27 \
  --dry-run
```

Apply from a fleet-reachable runner:

```text
gregalectl deploy join-node \
  --manifest-file /secure/fleet/manifest.yaml \
  --node fsn-3 \
  --ssh-host 203.0.113.27 \
  --ssh-user root \
  --ssh-key /secure/ssh/faas-fleet \
  --release-tarball /secure/releases/release.tar.gz \
  --bootstrap-binary /secure/tools/gregalectl-linux-amd64 \
  --cosign-binary /secure/tools/cosign-linux-amd64 \
  --pki-dir /secure/fleet/pki \
  --sign-key /secure/fleet/sign.key \
  --verify-key /secure/fleet/sign-pub.pem \
  --compute-db-env /secure/fleet/compute-db.env \
  --ansible-vars-file /secure/fleet/overlay-vars.yml \
  --yes
```

The `fleet.hosts[].address` value is not replaced with the provider's public
SSH address. It remains the stable private runtime endpoint and certificate
identity. `--ssh-host` is a connection override for this one adoption run.

## Scale-out operation

The manifest remains the desired fleet topology. Add the new stable private
hostname and runtime port to the manifest, prepare its PKI material, and run
one join command. No `host_vars` file, `hosts.ini` entry, provider API call, or
per-cloud code is required. At larger fleet sizes, run a complete preflight
once and use `--skip-fleet-preflight` only when the fleet facts are still
current; the join itself remains limited to the new node.

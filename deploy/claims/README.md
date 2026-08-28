# Compute node claims

`ComputeNodeClaim` is the provider-neutral handoff between creating a bare
metal machine and adopting it into Gregale. A provider or operator supplies
the connection facts once; the signed release, deployment manifest, PKI,
secrets, Ansible roles, and database lifecycle remain owned by Gregale.

The claim deliberately contains only:

- `metadata.name`: the compute-only node name already declared by the signed
  production manifest;
- `spec.ssh`: the provider's temporary connection address, user, and port;
- `spec.storage`: an optional stable device path and an explicit format flag.

It must not contain passwords, private keys, database DSNs, certificates, or
runtime daemon addresses. The SSH address is a connection-only override and is
never written into the node's runtime topology.

Validate a claim before using it:

```sh
gregalectl deploy claim validate \
  --file deploy/claims/compute-node.example.yaml
```

When a signed production manifest is available, also verify that the claim
names a declared `compute-only` host:

```sh
gregalectl deploy claim validate \
  --file /secure/fsn-4.yaml \
  --manifest-file /secure/production-manifest.yaml \
  --json
```

## GitHub Actions

The `cd-compute` workflow accepts `claim_file` as the preferred dispatch
input. The path is resolved from the selected release source, validated on a
GitHub-hosted preflight runner, and its normalized values are passed to the
trusted `faas-fleet` runner. This reduces the dispatch to:

```text
release_tag=v0.1.18-rc.10
claim_file=deploy/claims/fsn-4.yaml
```

The claim must be present in the selected release source. This keeps the
claim and the signed release auditable as one immutable deployment request.
The legacy `node`, `ssh_host`, and storage inputs remain available while
existing operators migrate.

## Direct CLI

For a bounded batch, `deploy join-fleet` can consume a single claim directly:

```sh
gregalectl deploy join-fleet \
  --claim-file /secure/fsn-4.yaml \
  --manifest-file /secure/production-manifest.yaml \
  --artifact-dir /secure/join-artifacts \
  --yes
```

`storage.format: true` is rejected unless `storage.device` is also supplied.
Formatting remains an explicit operator decision; the remote storage role
still refuses unsafe, mounted, or non-blank targets.

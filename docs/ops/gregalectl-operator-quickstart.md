# `gregalectl` operator quickstart

One-page operator reference for the operator-side binary. The
customer-side binary is `gregale` (sealed to the customer CLI surface
since PR-6.5). For the full first-time cutover, see
[`docs/runbooks/manifest-renderer-cutover.md`](../runbooks/manifest-renderer-cutover.md).

> Cross-links:
> - The split-box manifest schema: [`deploy/manifest/examples/splitbox.example.yaml`](../../deploy/manifest/examples/splitbox.example.yaml).
> - The cutover runbook (covers the per-host bootstrap chain + the
>   PR-X `secrets init` gap): [`docs/runbooks/manifest-renderer-cutover.md`](../runbooks/manifest-renderer-cutover.md).
> - The cluster architecture: [`docs/adr/110-declarative-split-box-manifest.md`](../adr/110-declarative-split-box-manifest.md).

## 1. Install `gregalectl`

Curl + SHA-256 pin (per `pkg/webhook/sealbytes-namespace.md`):

```
curl -fsSL https://dl.gregale.dev/cli/gregalectl/<git-sha>/gregalectl.linux-amd64 \
    -o /usr/local/bin/gregalectl
echo "<sha256-of-binary>  /usr/local/bin/gregalectl" | sha256sum -c -
chmod 0755 /usr/local/bin/gregalectl
```

The `<git-sha>` and `<sha256-of-binary>` come from the release
artifact published by `cd-controlplane.yml` (PR-1 rewired CD). The
artifact is bit-identical to the per-release `bin/gregalectl` under
`/opt/faas/releases/<sha>/bin/`.

## 2. Write and validate the manifest

Copy the cluster-shipped example and replace the illustrative host
endpoints with the routable address of each box. The endpoint is the
single source for SSH/Ansible reachability and the overlay address. The
generated compute target keeps the private PKI identity (`vmmd.faas`) and
uses the endpoint port; the generator maps literal mesh IPs to the
`vmmd.faas` / `schedd.faas` aliases in `/etc/hosts`.

```
cp deploy/manifest/examples/splitbox.example.yaml deploy/manifest/splitbox.yaml
$EDITOR deploy/manifest/splitbox.yaml
gregalectl manifest validate --file deploy/manifest/splitbox.yaml
```

## 3. Generate the Ansible inventory and bootstrap the boxes

```
make manifest-ansible MANIFEST=deploy/manifest/splitbox.yaml
```

The generated inventory owns `ansible_host`, node identity, private
service aliases, and `faas_vmmd_target_url`. A new bare-metal host
therefore requires a manifest change and regeneration, not a hand-edited
IP in the repository. Public Cloudflare DNS remains separate from this
private mTLS path; hostname endpoints require an operator-managed private
DNS entry for the role alias.

## 4. Bootstrap the box

```
# control-plane box (fsn-1)
sudo make ANSIBLE_INVENTORY=deploy/ansible/.generated/inventory/hosts.ini bootstrap-control-plane

# compute-only box (fsn-2)
sudo make ANSIBLE_INVENTORY=deploy/ansible/.generated/inventory/hosts.ini bootstrap-compute
```

These targets run `deploy/ansible/bootstrap.yml` against the
manifest-generated split-box inventory. There is no combined `[box]`
target: production hosts must be assigned exactly one role.

The schema docs are at
[`docs/adr/110-declarative-split-box-manifest.md`](../adr/110-declarative-split-box-manifest.md).

## 5. Validate + render

```
# Schema + cross-key validation
gregalectl manifest validate --file deploy/manifest/splitbox.yaml

# Dry-run — prints the planned writes without touching disk
gregalectl manifest render \
    --manifest-file deploy/manifest/splitbox.yaml \
    --host $(hostname) \
    --dry-run

# For-real — writes /etc/faas/*.toml, systemd units, cgroup scopes,
# and the per-box PKI subset
gregalectl manifest render \
    --manifest-file deploy/manifest/splitbox.yaml \
    --host $(hostname)
```

## 6. Install the release bundle

```
gregalectl release install --git-sha <git-sha>
```

Flips `/opt/faas/current` to the new SHA, lands the daemons under
`/opt/faas/releases/<sha>/bin/`, writes the `release_bundles` row,
and upsersts `compute_nodes.release_id` for the local box.

## 7. Doctor

```
gregalectl doctor
```

Exit codes:

| Code | Meaning |
|---|---|
| 0 | Healthy — no error findings |
| 1 | Usage error (mutually-exclusive flag combo) |
| 3 | Drift detected — findings in the report |

Flags:

- `--node NAME` — filter to a single `compute_nodes.name` row.
- `--release SHA` — filter to a single `release_bundles.git_sha`.
- `--deep` — re-hash on-disk daemon binaries against the bundle
  row. Slow on large fleets; the per-box check is the same as the
  release-bundle install path's `Verify`.
- `--fail-on {warn,error}` — exit non-zero threshold (default
  `error`).
- `--json` — machine-readable report.

## 7. Where to go next

- **First-time cutover from a legacy single-box?** Read
  [`docs/runbooks/manifest-renderer-cutover.md`](../runbooks/manifest-renderer-cutover.md).
- **Adding a second compute node to a working split-box fleet?**
  Read [`docs/runbooks/multi-host-rollout.md`](../runbooks/multi-host-rollout.md).
- **Troubleshooting drift?** Run `gregalectl doctor --deep` and
  read the JSON report; the `target` field on each finding points
  at the object (node name, git_sha, daemon name).

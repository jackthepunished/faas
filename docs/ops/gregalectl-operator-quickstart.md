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

## 2. Bootstrap the box

```
# control-plane box (fsn-1)
sudo make bootstrap-control-plane

# compute-only box (fsn-2)
sudo make bootstrap-compute
```

These targets run `deploy/ansible/bootstrap.yml` against the
three-group inventory (`deploy/ansible/inventory/hosts.ini`). The
legacy single-box target (`make bootstrap`) still works against
127.0.0.1 for dev/lima — `[box:children]` aggregates the multi-box
groups.

Per-box role + node name + connection vars live in
`host_vars/faas-fsn-{1,2}.yml`. Edit those, not the inventory.

## 3. Write the manifest

Copy the cluster-shipped example and edit `nodes.host` / `nodes.fleet`
/ `release.git_sha`:

```
cp deploy/manifest/examples/splitbox.example.yaml deploy/manifest/splitbox.yaml
$EDITOR deploy/manifest/splitbox.yaml
```

The schema docs are at
[`docs/adr/110-declarative-split-box-manifest.md`](../adr/110-declarative-split-box-manifest.md).

## 4. Validate + render

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

## 5. Install the release bundle

```
gregalectl release install --git-sha <git-sha>
```

Flips `/opt/faas/current` to the new SHA, lands the daemons under
`/opt/faas/releases/<sha>/bin/`, writes the `release_bundles` row,
and upsersts `compute_nodes.release_id` for the local box.

## 6. Doctor

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

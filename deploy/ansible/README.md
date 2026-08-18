# `deploy/ansible/` — host bootstrap

`make bootstrap` runs this playbook against the box itself. It's the
M0 acceptance gate from spec §14:

> `make bootstrap` idempotent on fresh Ubuntu 24.04

## What it does

In order (each role is independent and verifies its own preconditions):

| Role | Spec § | What it touches | Idempotent because |
|---|---|---|---|
| `cgroups_v2` | §11 | asserts kernel cmdline | verify-only |
| `grub` | §11 | `/etc/default/grub`, sysctl | `creates:` sentinel, regex match |
| `lvm` | §8 | verify lv-system / lv-fc | verify-only |
| `xfs` | §8 | `/srv/fc/jail` tmpfs | `/etc/fstab` `update` |
| `firecracker` | §4.4 | `/usr/local/bin/{firecracker,jailer}`, `/srv/fc/base/vmlinux-6.1` | `creates:` + SHA-256 pin |
| `systemd_slices` | §13 | three `.slice` unit drops | `creates:` on each |
| `nftables` | §7 | `/etc/nftables.conf` | `creates:` + `nft -c` syntax check |
| `postgres` | §1 (cp slice), §4 | postgres-15, `faas` user | apt idempotent, `creates:` on home |

## Run it

```
sudo apt update && sudo apt install -y ansible git
git clone <repo> faas && cd faas
make bootstrap          # first run: many "changed"
make bootstrap          # second run: zero "changed" — idempotent proof
ansible-playbook -i deploy/ansible/inventory deploy/ansible/site.yml --check --diff
                       # dry run, great for PR review
```

### Bootstrap targets (issue #911 / ADR-110)

The three-group inventory (`deploy/ansible/inventory/hosts.ini`) maps
to three Makefile targets. All three run `bootstrap.yml`; the
inventory's `--limit` selects the host set:

| Target | Inventory `--limit` | Hosts | When |
|---|---|---|---|
| `make bootstrap` | `box` | `[box]` (legacy single-box dev/lima) | dev / lima / 127.0.0.1 |
| `make bootstrap-control-plane` | `control_plane` | fsn-1 (control-plane) | split-box provisioning (PG-1) |
| `make bootstrap-compute` | `compute_nodes` | fsn-2 (compute-only) | split-box provisioning (PG-1) |

PR-7 split the inventory to three groups so `bootstrap.yml`'s three
plays (control_plane / compute_nodes / box) all match a host set. The
legacy `make bootstrap` against 127.0.0.1 still works because
`[box:children]` aggregates `[control_plane]` + `[compute_nodes]`.

For a split-box fleet, derive the inventory and per-host variables from
the manifest instead of editing committed IPs:

```
make manifest-ansible MANIFEST=deploy/manifest/splitbox.yaml
make ANSIBLE_INVENTORY=deploy/ansible/.generated/inventory/hosts.ini bootstrap-compute
```

The generated `host_vars` owns `faas_box_role`, `faas_node_name`,
`ansible_host`, `faas_vmmd_target_url`, and the private service aliases
written by the overlay role. `faas_vmmd_target_url` is
`tcp://vmmd.faas:<port>` so the existing internal PKI hostname check
remains enabled; `/etc/hosts` maps `vmmd.faas` and `schedd.faas` to the
manifest overlay addresses. The committed
`host_vars/faas-fsn-{1,2}.yml` files remain compatibility fixtures for
the legacy inventory and one-box development path.

## Do NOT run this on a non-bare-metal host without reading this section

The XFS `prjquota` requirement and the LVM `lv-system`/`lv-fc`
naming come from the reference host's `installimage` recipe (the
financial model ties the snapshot budget to a 2×512 GB RAID-1 layout).
The `lvm` role defaults to `faas_storage_layout=auto`: hosts with the
reference LVM volumes are validated, while provider-native disks such as
GCP persistent disks use their filesystem directly. Set
`faas_storage_layout=reference-lvm` when a fleet requires the reference
layout. The `xfs` role similarly enforces `prjquota` only when `/srv/fc`
is mounted as a real filesystem.

## After the reference node hosts the executor

Wire `self-hosted, kvm` label to the runner and the existing
`.github/workflows/ci.yml` `metal` job flips on automatically — its
`if: false` guard only stops it running on stock GitHub runners, not
when the right hardware is registered.

Verify end-to-end:

```
sudo make test-metal   # boots a hello-Firecracker VM via the pinned kernel + busybox
sudo make leakcheck    # asserts zero leaked netns / taps / jails / cgroups
```

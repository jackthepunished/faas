# `deploy/ansible/` — host bootstrap

`bootstrap.yml` is the only production host bootstrap playbook. It has
separate control-plane and compute-only plays and refuses a single-box role.

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
| `postgres` | §1 (cp slice), §4 | distro PostgreSQL major, `faas` user | apt idempotent, `creates:` on home |

## Run it

```
sudo apt update && sudo apt install -y ansible git
git clone <repo> faas && cd faas
make manifest-ansible MANIFEST=/path/to/splitbox.yaml
make ANSIBLE_INVENTORY=deploy/ansible/.generated/inventory/hosts.ini bootstrap-control-plane
make ANSIBLE_INVENTORY=deploy/ansible/.generated/inventory/hosts.ini bootstrap-compute
```

### Bootstrap targets (issue #911 / ADR-110)

The split-box inventory maps to two Makefile targets:

| Target | Inventory `--limit` | Hosts | When |
|---|---|---|---|
| `make bootstrap-control-plane` | `control_plane` | fsn-1 (control-plane) | split-box provisioning (PG-1) |
| `make bootstrap-compute` | `compute_nodes` | fsn-2 (compute-only) | split-box provisioning (PG-1) |

There is intentionally no combined `[box]` group. A host must belong to
exactly one production role group, and `role_convergence` verifies that the
host variable, inventory group, systemd role drop-ins, and active service
set agree. The image builder uses
`deploy/packer/inventory/image-seed.ini` only while baking a role-agnostic
image; it is not a production inventory.

For a split-box fleet, derive the inventory and per-host variables from
the manifest instead of editing committed IPs:

```
make manifest-ansible MANIFEST=deploy/manifest/splitbox.yaml
make ANSIBLE_INVENTORY=deploy/ansible/.generated/inventory/hosts.ini bootstrap-compute
```

The generated `host_vars` owns `faas_box_role`, `faas_node_name`,
`ansible_host`, `faas_vmmd_target_url`, and the private endpoint records
written by the overlay role. `faas_vmmd_target_url` is
`tcp://vmmd.faas:<port>` so the existing internal PKI hostname check
remains enabled. The bootstrap's discovery play gathers one private address
per host, and the overlay role maps the fleet names plus `vmmd.faas`,
`schedd.faas`, and `egress.faas` in one managed `/etc/hosts` block. Providers
whose default route is public set `faas_private_address` in provider-owned
inventory; daemon URLs and the committed manifest remain unchanged. The committed
`host_vars/faas-fsn-{1,2}.yml` files remain checked-in reference fixtures;
the manifest-generated tree is the deployment source of truth.

For a split-box manifest, the generated control-plane variables also
declare the database listener address and the compute `/32` allow-list.
Provide `faas_postgres_password` through Ansible Vault (or another secret
source) before bootstrapping; the role refuses to expose PostgreSQL without
that password. The manifest's `postgresql.dsn` must use the same control-plane
mesh address, not `127.0.0.1` or a local Unix socket.

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

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

## Do NOT run this on a non-bare-metal host without reading this section

The XFS `prjquota` requirement and the LVM `lv-system`/`lv-fc`
naming come from the reference host's `installimage` recipe (the
financial model ties the snapshot budget to a 2×512 GB RAID-1 layout).
On hosts without compatible storage the `lvm` and `xfs` roles will
`fail` with explicit remediation steps — that's intentional.

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

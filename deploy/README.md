# deploy/ — host provisioning and runtime config

Bootstraps a fresh bare-metal x86_64 split-box fleet to Gregale-ready:

```
make manifest-ansible MANIFEST=deploy/manifest/splitbox.yaml
make ANSIBLE_INVENTORY=deploy/ansible/.generated/inventory/hosts.ini bootstrap-control-plane
make ANSIBLE_INVENTORY=deploy/ansible/.generated/inventory/hosts.ini bootstrap-compute
```

then verify the platform works end-to-end:

```
sudo make test-metal    # `go test -tags metal ./...` — boots a hello-Firecracker VM
sudo make leakcheck     # asserts zero leaked netns/taps/jails/cgroups
make build              # compile every daemon
make test               # cross-platform unit tests
```

- `ansible/` — role-aware split-box bootstrap: the control-plane and
  compute-only plays install only their own daemon set and mask stale
  opposite-role services. See [`ansible/README.md`](ansible/README.md).
- `systemd/` — one unit + slice per daemon; memory.max fences the RAM
  ledger. Wired up in M5 (per-slice `.slice` units land in
  `ansible/roles/systemd_slices/`).
- `nftables/` — tenant + builder egress policy (§7): deny 25/465/587,
  deny RFC1918/link-local/metadata. Dropped as `/etc/nftables.conf`
  via the `nftables` ansible role.
- `scripts/` — ops helpers (`leakcheck.sh` for the shell-side check,
  restore drill planned for M8).
- `controlplane/` — retired legacy bootstrap surface. Production hosts use
  the manifest-generated split-box inventory and role-aware Ansible targets;
  the image builder uses its isolated image-seed inventory.

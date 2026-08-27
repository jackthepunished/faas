# storage role

This role owns the provider-neutral /srv/fc contract used by Firecracker:

- XFS with reflink enabled;
- noatime,prjquota mount options;
- UUID-based /etc/fstab entry;
- the standard base/, snap/, layers/, jail/, and staging directories.

It does not call a provider API. The host is expected to have already been
created and to be reachable over SSH. The role discovers Linux block devices
instead:

- a valid existing /srv/fc filesystem is reused;
- one blank non-root disk is formatted directly;
- two equally sized blank non-root disks are mirrored with mdadm and then
  formatted;
- mounted, signed, partitioned, or ambiguous disks are never reformatted
  automatically.

The default faas_storage_mode: auto is safe for the join pipeline. A
provider-owned image may set faas_storage_mode: validate-only when storage
is deliberately prepared outside Ansible. faas_storage_device is available
as an explicit escape hatch for a host with more than two eligible blank
disks; it should use a stable /dev/disk/by-id path, not /dev/sdX.

The default discovery threshold is 50 GiB. It is a selection guard rather
than a capacity guarantee; operators can raise it for a larger production
fleet or lower it for a deliberately small test machine.

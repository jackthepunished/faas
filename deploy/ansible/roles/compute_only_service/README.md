# compute_only_service ansible role

Drops the imaged systemd unit + example TOML + FAAS_NODE_NAME/FAAS_IMAGED_ROLE
drop-ins. Does NOT enable or start imaged — the operator runs
`systemctl enable --now faas-imaged` once `/etc/faas/sealed.env` is populated
with `DATABASE_URL` (gap G2).

## Drop-ins

- `99-faas-node-name.conf.j2` (linked from `_shared/`) — exposes this box's
  compute_node identity to imaged (and builderd, when co-installed on the
  compute-only box) so the role gate + boot log carry the right name.
  imaged has no `config.go` today (env-only); the env is the only source.
- `99-faas-role.conf.j2` — wires the per-box role gate through to imaged via
  `FAAS_IMAGED_ROLE` so `cmd/imaged/main.go::LoadConfig` picks the right
  `role.FromConfig` sentinel. Without this drop-in imaged falls back to
  `RoleSingleBox` on the compute-only box and the cosign sign-keypair path
  assumes single-box assumptions (no per-box PKI subset).

## Side effects

- Creates the `faas` group (shared with the vmmd role).
- Ensures `/etc/faas` exists (mode 0750 root:faas).
- Installs the imaged example TOML to `/etc/faas/imaged.toml.example`
  (operator copies to `imaged.toml`).
- Installs the systemd unit to `/etc/systemd/system/faas-imaged.service`.

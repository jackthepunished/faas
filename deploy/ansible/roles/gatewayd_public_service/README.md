# gatewayd_public_service ansible role

Installs the systemd unit and storage layout for `gatewayd-public`,
the TLS-only edge daemon introduced in the Tier A7 split
(ADR-070). This role replaces the legacy `gatewayd_service` role
for new installs; operators on the legacy daemon can run both side
by side during the migration window.

## What this role does

1. Installs `/var/lib/faas/certs` (mode 0700, faas:faas) for
   CertMagic per-replica storage.
2. Installs `/var/lib/faas/ca` (mode 0750, faas:faas) for the
   shared ACME account directory.
3. Drops `/etc/systemd/system/faas-gatewayd-public.service` and
   runs `systemctl daemon-reload`.

## What this role does NOT do

- Provision `FAAS_NODE_ID` / `FAAS_NODE_NAME` env vars (operator
  drop-in at `/etc/systemd/system/faas-gatewayd-public.service.d/`).
- Provision `FAAS_HETZNER_DNS_TOKEN_PATH` (operator secrets dir).
- Enable the unit (`systemctl enable --now faas-gatewayd-public`).
- Run the daemon (CertMagic mints certs on first start; we don't
  pre-warm them).

## See also

- `docs/adr/068-tier-a7-edge-split.md`
- `deploy/systemd/faas-gatewayd-public.service`
- `cmd/gatewayd-public/main.go`
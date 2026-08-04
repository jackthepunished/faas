# gatewayd_public_service ansible role

Installs the systemd unit for `gatewayd-public`, the plain-HTTP
edge daemon introduced by the Tier A7 split (ADR-070). TLS
terminates at Caddy + Cloudflare upstream (`api.gregale.dev`); this
daemon forwards plaintext requests to `gatewayd-internal` over
`/run/faas/gatewayd-internal.sock`.

## What this role does

1. Drops `/etc/systemd/system/faas-gatewayd-public.service` and
   runs `systemctl daemon-reload`.

## What this role does NOT do

- Provision any certmagic storage (`/var/lib/faas/certs` is removed;
  TLS is upstream).
- Provision an ACME account directory (`/var/lib/faas/ca` is
  removed; no DNS-01 here).
- Provision `FAAS_HETZNER_DNS_TOKEN_PATH` (no DNS-01 here).
- Provision `FAAS_NODE_ID` / `FAAS_NODE_NAME` env vars (operator
  drop-in at `/etc/systemd/system/faas-gatewayd-public.service.d/`
  if ever needed).
- Enable the unit (`systemctl enable --now faas-gatewayd-public`).
- Run the daemon.
- Configure the upstream Caddy reverse-proxy to
  `http://127.0.0.1:8080` (Caddy is provisioned by the operator or
  by a separate role; see `docs/ops/gatewayd-caddy-upstream.md`).

## See also

- `docs/adr/068-tier-a7-edge-split.md`
- `deploy/systemd/faas-gatewayd-public.service`
- `cmd/gatewayd-public/main.go`
# `node_exporter` ansible role

Installs the Prometheus `node_exporter` binary pinned to a specific
version, bound to loopback (`127.0.0.1:9100`). Prometheus scrapes
locally, so the public iface's input chain never needs to expose host
telemetry on a provider-specific bridge.

## Why loopback-only

`nftables` host policy (spec §7) drops unsolicited ingress on the
public iface. The Prometheus scraper dials from inside the box, so
listening on the public iface or a provider-specific bridge would be
additional attack surface for zero benefit.

## Collectors disabled

- `--collector.filesystem.mount-points-exclude=^/(sys|proc|dev|run)($|/)`
  — skip pseudo-fs noise.
- `--collector.netclass.ignored-devices=^(veth|docker)` — skip
  container veth pairs (we don't run containers on the host;
  cgroups v2 hosts are isolated by slice instead).
- `--collector.diskstats.ignored-devices=^(loop|dm-)` — skip loop
  + device-mapper noise (loop is the installer; dm-* is lvm, which
  we expose via the custom `fcvm_lv_fc_used_pct` gauge instead).

## Override at invocation

```bash
ansible-playbook -e node_exporter_version=1.9.0 \
                 -e node_exporter_release_sha256=<new-sha> bootstrap.yml
```

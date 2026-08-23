# H2C bridge rollout

Operator runbook for turning on the bridge-side H2C terminator
(ADR-126 / PR #1050) and its hardening follow-on (ADR-127 / G19.1)
on a Gregale control-plane node. The wire-shape is **opt-in per app**
via `apps.app_protocol ∈ {http2, grpc}`; this runbook is about
**flipping the bridge default** so newly-adopted apps reach the
guest's `:8080` as native H2 frames (and `app_protocol=grpc` apps
reach it as native gRPC trailers) without per-vmmd surgery.

## Audience

On-call SRE (PagerDuty schedule `faas-platform-oncall`). The
five-step procedure below takes ~20 min per box; the gating
decisions are at step 3 (smoke) and step 4 (promotion-to-default).
Everything else is mechanical.

## Pre-conditions

- **Release ≥ PR #1050 + ADR-127** installed on the box. Confirm
  with `dpkg -l faas-vmmd | grep -E '^[a-z].+[0-9].+'` — the package
  version string embeds the VCS tag; the H2C terminator landed in
  tag `0.7.6-faas-h2c-bridge` and the hardening overlay in
  `0.7.7-faas-g19.1-bridge-hardening`.
- **`apps.app_protocol` column exists.** Confirm with:
  ```sh
  psql -tA "$FAAS_DATABASE_URL" \
    -c "select column_name from information_schema.columns \
        where table_schema='public' and table_name='apps' \
        and column_name='app_protocol';"
  # expect: app_protocol
  ```
  Migration `00382_apps_app_protocol.sql` adds it; if missing the
  fleet is pre-ADR-124 and must upgrade first.
- **`apps_app_protocol_chk` CHECK constraint in place.** Same
  query, swap `column_name` for `constraint_name` and table for
  `table_constraints`. A `failed app_protocol` row in the bridge
  log line is the warning sign the CHECK did not run.
- **Base image at `FAAS_BASE_IMAGE_VERSION = "v1"`.** The constant
  lives at `pkg/fcvm/snapshot.go::FAAS_BASE_IMAGE_VERSION` — it is a
  Go const, **not** a database row, so there is no
  `apid_settings.FAAS_BASE_IMAGE_VERSION` to query. Confirm via one
  of:
  ```sh
  # Option A — grep the deployed vmmd binary (Go consts are encoded
  # in the binary data section as ASCII strings).
  strings /usr/bin/faas-vmmd | grep -F 'FAAS_BASE_IMAGE_VERSION = "'
  # expect: FAAS_BASE_IMAGE_VERSION = "v1"

  # Option B — read the source bundle if the operator has it.
  grep -r 'FAAS_BASE_IMAGE_VERSION =' pkg/fcvm/snapshot.go
  # expect: const FAAS_BASE_IMAGE_VERSION = "v1"

  # Option C — check the deb/rpm package version. Tag >= 0.7.7-faas-
  # g19.1-bridge-hardening ships "v1"; pre-v1 tags ship "" or omit
  # the const entirely.
  dpkg -l faas-vmmd | awk '/^ii/ {print $3}'
  ```
  Pre-v1 images speak H1 only; promoting the bridge default with
  pre-v1 images leaves `app_protocol in {http2, grpc}` apps with a
  wire-shape downgrade.
- **Grafana + Prometheus provisioning live.** `FaasBridgeFramingMismatch`
  alert rule must be in `deploy/ansible/roles/prometheus/files/bridge.rules.yml`
  and the `bridge-protection` dashboard JSON in
  `deploy/ansible/roles/grafana/files/bridge-protection.json`. Mirror
  parity is enforced by `make grafana-mirror-check`; if the
  `bridge-protection` rows in your `prometheus.yml.j2` are missing,
  re-run `make bootstrap-prometheus`.

## Procedure

### Step 1 — Grafana baseline (5 min)

Open the `bridge-protection` dashboard and pin the four panels:

1. **Bridge framing rate by (app_protocol, bridge_protocol, framing) / 5m.**
   Pre-`FAAS_BRIDGE_PROTOCOL=h2c` deployment the panel reads 0 across
   the closed cross-product — the pre-instantiated series surface a
   zero row from idle fleet (commit 9 wired this contract).
2. **Bridge framing MISMATCH rate / 5m.** Same — 0.
3. **Active `bridge_protocol=h1` (surgical-rollback apps).** Same — 0.
4. **Bridge H2C handshake latency p99 (s).** Same — no data (panel
   references `vmmd_op_duration_seconds_bucket{op="bridge_h2c_roundtrip"}`,
   which lands with a follow-on PR; the panel is wired so a single
   follow-on commit closes the gap).

Bookmark the dashboard. The rollout's success criterion is panel 2
staying at 0 and panel 1's `app_protocol=http2|grpc` rows climbing
without `mismatch` entries.

### Step 2 — Flip the per-vmmd env var (10 min per box, box-by-box)

The bridge reads `FAAS_BRIDGE_PROTOCOL` per-request via
`cmd/vmmd-stream-bridge/framing.go::currentBridgeFraming()`. The
canonical pattern is per-vmmd `systemd` override, no app restart
needed:

```sh
sudo systemctl edit faas-vmmd
# In the editor, add:
#   [Service]
#   Environment=FAAS_BRIDGE_PROTOCOL=h2c
sudo systemctl daemon-reload
sudo systemctl restart faas-vmmd
```

Verify the framing-selection slog line:

```sh
sudo journalctl -u faas-vmmd -f | grep 'framing selected'
# expect: framing=h2c app_protocol_env=h2c method=POST path=/<route> ...
```

`app_protocol_env=h2c` AND `framing=h2c` is the green state. Anything
else and panel 2 will start ticking red within the 1h alert window.

### Step 3 — Smoke (5 min)

Pick one Hobby+ app with `app_protocol=http2` (or patch a test app
to `http2` per the §4.1 plan gate) and:

```sh
# 1. Confirm the bridge picks h2c on the wire.
curl -v --http2-prior-knowledge https://<app-hostname>/ 2>&1 \
  | grep -E '^\* (Using HTTP2|ALPN|Sending request|Stream id)'
# expect: '* Using HTTP2', '* Server certificate OK'

# 2. Confirm the vmmd-side framing matches.
sudo journalctl -u faas-vmmd --since='-1m' \
  | grep 'framing selected' \
  | tail -5
# expect: framing=h2c app_protocol_env=h2c

# 3. Confirm the dashboard panel reflects the wire-shape.
# Panel 1 row "http2/h2c/match" should climb > 0 ops/5m within 1m
# of the curl. Panel 2 (mismatch) should stay at 0.
```

If panel 2 starts ticking red, **stop the rollout** and consult
`docs/ops/h2c-rollback.md` Switch 1 (`FAAS_BRIDGE_PROTOCOL=h1`).
Do not promote-to-default until a 24h clean window passes.

### Step 4 — Promote to default (1 box, 24h, then fleet)

Per-vmmd `FAAS_BRIDGE_PROTOCOL=h2c` is not the same as "the fleet
default." The fleet default flips when vmmd ships without the env
override and `framing.go`'s default in `currentBridgeFraming()`
returns `"h2c"` instead of `"h1"`. The promotion-to-default
checklist:

1. **24h clean window** on the box from step 3:
   - Panel 2 stays at 0.
   - Panel 1's `http2/h2c/match` and `grpc/h2c/match` rows track
     traffic without `mismatch` rows.
   - No `bridge panic:` lines in `journalctl -u faas-vmmd` (Layer 9
     `defer recover()` logs each one).
   - No `bridge_h2c_roundtrip` histogram outliers in Panel 4 once
     the histogram ships.
2. **Box-by-box fleet rollout.** Repeat steps 2–3 on each subsequent
   box, one at a time, watching the dashboard per box.
3. **Promote in source** — flip the default in
   `cmd/vmmd-stream-bridge/framing.go::currentBridgeFraming()`
   (and in `pkg/vmmdgrpc/forward.go::appProtocolToBridgeProtocol`),
   ship in the next release, drop the env overrides.

### Step 5 — Post-rollout

- Append the box's promotion date + cluster to this runbook's
  [Rollout history](#rollout-history) section (kept empty by
  default; first promotion appends a row).
- Update `docs/STATUS.md` M8.6 row with the promotion date if you
  flipped the source default in step 4.3 (commit-level history is
  on the PR; STATUS.md row stays at 🚧 until step 4.3 ships).
- The two surgical-rollback switches in `docs/ops/h2c-rollback.md`
  stay live forever — promotion does not retire them.

## Rollout history

Append a row per box promotion; keep column headers stable so a
downstream `gh-actions/audit-rollout.py` can parse.

| Date | Cluster | Box | Pre-flight | Post-24h panel 2 | Source default flipped? | Notes |
|------|---------|-----|------------|------------------|-------------------------|-------|
| (empty) | — | — | — | — | — | first promotion appends a row |

## Cross-links

- Spec §4.1 line 115 — `docs/faas_implementation_spec.md` (rewrite
  describes ADR-126 wire-shape + ADR-127 hardening pair).
- ADR-126 — `docs/adr/126-bridge-h2c-terminator.md` (wire-shape).
- ADR-127 — `docs/adr/127-g19.1-h2c-bridge-hardening.md` (hardening).
- STATUS.md M8.5 — wire-shape shipped, §17 G19 RESOLVED.
- STATUS.md M8.6 — hardening overlay (this PR).
- `docs/ops/h2c-rollback.md` — companion rollback runbook.

## Escalation

- Panel 2 (`bridge_framing_total{framing="mismatch"}`) sustained
  > 0.1 rps for > 1h → `FaasBridgeFramingMismatch` warn alert fires
  → page #oncall-faas.
- Panel 2 sustained for > 4h → `FaasBridgeRollbackStuck` page alert
  fires → execute `h2c-rollback.md` Switch 1 (per-vmmd surgical).
- `bridge panic:` in `journalctl -u faas-vmmd` for any reason →
  page immediately; the `defer recover()` is a defense-in-depth
  not a fix.
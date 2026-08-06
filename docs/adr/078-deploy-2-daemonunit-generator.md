# ADR-078: pkg/daemonunit + pkg/daemonunitspec generator (DEPLOY-2)

## Status

Accepted. Issue #649.

## Context

Systemd unit files for the eight production daemons lived in three
different trees, all hand-edited and all drifted:

- `deploy/systemd/` — `make systemd` source for legacy + dev VMs
  (10 services, 2 timers, README). Skips `githubd` and `meterd`
  (cp-cp only).
- `deploy/controlplane/systemd/` — what `cd-controlplane.yml`
  actually installs on the box (8 services + `faas-cp.slice`).
- `deploy/ansible/roles/control_plane_service/files/` — what the
  ansible `control_plane_service` role drops in (4 services: apid,
  imaged, meterd, schedd). Doesn't ship vmmd, gatewayd-internal,
  gatewayd-public, or githubd.

The drifts were real and load-bearing-but-not-captured. Examples
that fired in production:

- `Requires=postgresql.service` declared on every daemon, but
  Postgres is started out of band by ansible (CLAUDE.md component
  ownership: "components talk via Postgres rows + pg_notify"). The
  Requires= line is dead documentation; systemd can't enforce it
  because the unit isn't in the graph.
- imaged's `AmbientCapabilities=cap_sys_admin` was cleared in the
  cp-sys tree post-DEPLOY-1 / ADR-075 but lingered in cp-cp +
  cp-ans. cp-cp's imaged crash-looped on overlay mounts because
  vmmd no longer honoured that cap (ADR-075: the parent-ref mount
  is now an RPC to vmmd, and imaged is supposed to deny
  `cap_sys_admin`).
- gatewayd-internal's `RestrictAddressFamilies` differed: cp-sys
  unix-only (`AF_UNIX`); cp-cp + cp-ans added `AF_INET`/`AF_INET6`
  for `127.0.0.1` listener. cp-sys boots wouldn't include the
  listener that the cp-cp deploy expects.
- ExecStart flag sweep (PR-M.4 era): cp-cp shipped
  `--config /etc/faas/<daemon>.toml` on a daemons that don't have
  a toml; cp-sys had stale flags. Fixed by hand across three trees
  in PR #632.
- gatewayd-public `:9090` collision (PR-J): the public listener
  collided with gatewayd-internal because neither tree's toml
  existed — a hand-write that drifted.

The recurring pattern: every fix landed in 1–3 trees, the
remaining trees diverged, and the next cd-controlplane deploy
tripped over the divergence. The fix was always a manual sweep.

## Decision

### Single source of truth: `pkg/daemonunitspec`

8 files, one Go package, one function per daemon:

```go
func UnitApid() daemonunit.Unit { ... }
func UnitVmmd() daemonunit.Unit { ... }
func UnitSchedd() daemonunit.Unit { ... }
func UnitGatewaydPublic() daemonunit.Unit { ... }
func UnitGatewaydInternal() daemonunit.Unit { ... }
func UnitMeterd() daemonunit.Unit { ... }
func UnitGithubd() daemonunit.Unit { ... }
func UnitImaged() daemonunit.Unit { ... }

var Registry = []daemonunitspec.Entry{ ... }
```

`pkg/daemonunit` (leaf package, no `pkg/daemonunitspec` import)
holds the `Unit` struct + `Render()` + `Decode()` + `Diff()`. The
8 daemon spec files each declare a `daemonunit.Unit` literal that
captures every directive the unit ships with.

### Generator: `cmd/deployctl`

Subcommands:

- `deployctl generate` — emit units + `daemons.json` to all three
  default trees (`deploy/controlplane/systemd/`,
  `deploy/systemd/`,
  `deploy/ansible/roles/control_plane_service/files/`).
- `deployctl check` — regenerate to a tempdir, byte-compare
  against committed; exit 1 on drift. CI gate.
- `deployctl diff` — like `check` but prints the diff to stdout.

Per-tree skip sets:

- cp-cp: ship all 8 daemons + `faas-cp.slice` + `daemons.json`.
- cp-sys: ship 6 (no githubd, no meterd) + legacy artefacts
  preserved (`faas-gatewayd.service`, `pg-basebackup-*`, README.md).
- cp-ans: ship 4 (apid, imaged, meterd, schedd). Widening to 8
  is a separate ops change.

`daemons.json` shape:

```json
{
  "critical": ["apid", "gatewayd-internal", "gatewayd-public",
               "githubd", "meterd", "schedd", "vmmd"],
  "best_effort": ["imaged"]
}
```

`cd-controlplane.yml` reads the two arrays via
`jq -r '.critical | join(" ")' deploy/etc/daemons.json` and the
analogous `best_effort` query.

### Wipe comments; move rationale to godoc + this ADR

Hand-written units carried inline comments that explained the
**why** behind load-bearing choices (vmmd's `RuntimeDirectory=faas`
SOLE invariant, schedd's `PrivateTmp=no` rationale, imaged's
capability boundary). Comments are wiped from the generated
output; the rationale moves to:

1. The godoc on each `UnitXxx()` function in `pkg/daemonunitspec`.
2. This ADR (the architectural rationale that spans daemons).
3. The original spec sections / ADRs referenced from godoc.

The wipe is a deliberate decision: the generated units are now
diff-stable and parser-friendly; the rationale lives next to the
code that needs to change when the rationale changes.

### CI gate: `daemonunit-check`

`.github/workflows/ci.yml` runs `make generate-check` on every
PR. Failure ⇒ drift detected; the PR author must re-run
`make generate` locally, commit the regenerated trees, and push.
The gate is byte-exact; there is no "minor drift" tolerance,
because the entire point of the generator is to keep the trees
identical.

### Canonical = shipped runtime

Canonical values mirror the cp-cp tree (what the box runs).
Corrections baked in at generation time:

- Drop `Requires=postgresql.service` from every daemon (Postgres
  started out of band by ansible, not via systemd unit graph).
- imaged: no `AmbientCapabilities` (DEPLOY-1 / ADR-075 — capdecl
  on vmmd only; imaged's parent-ref mount is an RPC).
- vmmd: `RuntimeDirectory=faas` + `RuntimeDirectoryMode=0775`,
  plus the PR-M.2 `ExecStartPre=chown root:faas /run/faas` and
  `ExecStartPre=chmod 0775 /run/faas` lines (SOLE invariant).
- gatewayd-public: explicit empty `AmbientCapabilities=` body
  (locked-down; no caps elevated, not even for binding).
- apid: `ReadWritePaths=/var/lib/faas` (PR #661).
- apid: 3 LoadCredentials, with `faas_host_age_identity_previous`
  carrying the `:-` rotation-overlap optional flag.

## Why a Go generator, not a templating language

Considered: Go `text/template`, `gomplate`, hand-written strings,
a YAML DSL.

- `text/template`: no type safety; field-name typos don't surface
  until render. `Unit` struct with field-level `Render()` methods
  gives compile-time safety + tree-level tests.
- YAML DSL: schemas drift. The whole problem is that the three
  trees drifted; a YAML that lets the same shape appear in three
  forms is just three trees again, with a parser bolted on.
- Hand-written strings: this is the status quo, modulo the
  generator.

The `pkg/daemonunit` package is a leaf. Adding a daemon is one
`pkg/daemonunitspec/<name>.go` file + one registry entry; CI
fails until `make generate` is re-run. The blast radius is
small.

## Out of scope (follow-up issues)

- **DEPLOY-3 (issue #650)** — Go state-machine deploy replaces
  the bash workflow.
- **`deploy/controlplane/deploy.sh:32`** pre-A7 manual-deploy
  list — pre-existing drift.
- **`pg-basebackup-push.service` / timer** in `deploy/systemd/`
  — not daemons; stay hand-written.
- **Legacy `faas-gatewayd.service`** in both cp-cp and cp-sys —
  kept during A7 migration window.
- **Ansible role widening** (cp-ans ships all 8 daemons) —
  tracked separately.

## Memory updates

- `deploy-2-daemonunit-generator.md` (new) — single rule: "edit
  `pkg/daemonunitspec/<daemon>.go`, run `make generate`, commit
  both". CI `daemonunit-check` is the tripwire.
- Per-daemon cross-references in pkg/daemonunitspec godoc point
  to this ADR by number.

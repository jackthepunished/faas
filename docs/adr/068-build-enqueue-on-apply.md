# ADR-068: build enqueue on `POST /v1/projects` apply

## Status

Accepted — mega-PR for issue #527 (apply path closes the build
gap).

## Context

Repo decomposition (ADR-050) gives customers a single CLI
keypress to create a project + N apps from one tarball. Phases
1–5 are merged. Two entry paths reach the same reconcile core:

- **GitHub push** (`cmd/githubd`) reconciles the project AND
  enqueues builds per changed workload, path-filtered.
- **CLI apply** (`POST /v1/projects`) reconciles the project
  and **stops there**.

`pkg/reconcile/reconcile.go:74-77` documents `Result.BuildIDs`
as *"always nil in this mega-PR; PR-H fills it via the
path-filtered fan-out"* — a designed seam that was filled for
githubd only. `grep EnqueueBuild` hits nothing outside
`cmd/githubd/` + `cmd/apid/githubd_bridge.go`.

Customer-visible consequence: `gregale deploy` on a
multi-workload repo creates N app rows in `PARKED` whose source
is never built. `ApplyResponse` carries no build or deployment
IDs, and the CLI prints *"Created project … N app(s)"* and
exits. The single-app path right next to it
(`cmd/gregale/commands2.go:471+`) calls `DeployTarball` →
`streamDeployLogs`. The plan doc's own target experience says
apply "creates the project, four apps, one cron row, **and
enqueues four builds**". The fourth clause is false on the
CLI path.

The gap survived because there is no e2e test of the apply
path. `cmd/e2e/scan_project_e2e_test.go` covers the scan wire
contract thoroughly and its header explicitly disclaims apply.
Apply is tested only at unit level
(`pgstore_apply_plan_test.go`, `reconcile_test.go`) and via a
stubbed server in `cmd/gregale/commands_decompose_test.go`.
**Nothing takes a tarball through `POST /v1/projects` and
asserts what landed.**

## Decision

The apply path enqueues one build per created/changed workload,
matching the githubd path's wire contract. The two callsites
now share one helper.

### Shared helper

`pkg/apid/apidsource.Enqueue(ctx, store, notif, log, p)`
wraps the seven-step build-enqueue sequence
(LatestDeployment → CreateDeployment → mkdir log spool →
UpdateDeploymentStatus(building) → CreateBuild → two notifies).
`Store` and `Notifier` interfaces mirror the existing
`githubd_bridge.go:47-53` slice. Each caller keeps its own
auth preamble and error mapping (RFC 7807 vs gRPC `asGRPC`);
the helper returns plain wrapped errors. Three callers:

- `cmd/apid/deploy_inputs.go` — single-app tarball deploy.
- `cmd/apid/githubd_bridge.go` — gRPC path-filtered fan-out.
- `cmd/apid/scan_service.go::applyBuildsForAddedChanged` —
  multi-workload apply loop (NEW).

Behaviour must not change at the first two sites — the
existing tests are the regression net.

### Per-workload source staging

`POST /v1/projects` accepts one tarball holding N workloads;
each workload has a `RootDir` and needs its own tarball
rooted at it. `pkg/githubd/staging.go:126`
`repackageRootTree(ctx, src fs.FS, rootDir, dst)` already
does this as a generic free function over `fs.FS`; it was
unexported and gets re-exported as
`githubd.RepackageRootTree`. The barrier was purely the
lowercase identifier — `.golangci.yml`'s
`apid-control-plane-only` depguard block does not deny
`pkg/githubd`.

The apply path already has the extracted tree —
`req.ScanDir`, consumed as `os.DirFS(req.ScanDir)`. Pairing
is clean: write the per-workload tarball to
`<spoolRoot>/projects/<acct>/<project>/<appID>.tar.gz`
**before responding**, because the upload itself is
`defer os.Remove(req.SourcePath)`-ed at the end of the
request.

### Deployment kind

`deployments_kind_check` allows
`{image, tarball, dockerfile, github}`;
`builds_kind_check` allows
`{railpack, dockerfile, tarball, github}`. Both call sites
pass the *same* kind to `CreateDeployment` and `CreateBuild`,
so the intersection is the only safe value. **Use
`DeploymentKindTarball`.** builderd's detector picks Railpack
vs Dockerfile at build time; there is no
`DeploymentKindRailpack` constant.

Parity trap: `MemStore.CreateDeployment`
(`memstore.go:2324-2332`) defaults empty `Kind` → `image`;
`PgStore` does not, so an empty kind passes MemStore tests
and violates the PG CHECK. Pinned by an e2e test.

### Partial-failure semantics

Each `CreateDeployment` is its own tx; there is no
cross-app atomicity. **Match githubd: per-app partial success**
(`pkg/githubd/service.go:361-367` logs and continues).
Failing 50 provisioned apps because one build enqueue failed
is worse for the customer, and the durable net
(`ClaimNextQueuedBuild`, `FOR UPDATE SKIP LOCKED`) means a
missed `pg_notify` is recoverable anyway.

### Wire + CLI

`ApplyResponse` extended **additively**:
`Builds []AppliedBuild` with `omitempty`. `AppliedBuild`
carries `{slug, app_id, deployment_id, build_id, error}`.
`omitempty` keeps existing `--json` consumers and the
sdk-coverage gate stable. CLI (`cmd/gregale/commands2.go:466`)
gains a per-app build line.

**Out of scope, filed as follow-ups:** multi-app build-log
streaming (N concurrent log streams in one terminal);
spool GC for staged project tarballs.

### Adjacent bug fix

`req.ScanDir` is documented *"cleaned up by caller"* but **no
caller ever removes it**. `extractTarGzToDir` only `RemoveAll`s
on the error path. Every successful scan/apply leaked a
directory under `FAAS_SCAN_SPOOL_ROOT`. Add the missing
`defer os.RemoveAll(req.ScanDir)` in `scanService` — **after
staging**, since staging reads from it. Pinned by an e2e
test.

### No migration

Everything lands on existing columns; `tarball` is already
valid in both CHECKs. Avoids the slot-collision churn that has
bitten this repo repeatedly (memory: `cross-pr-slot-gate-…`).

## Test depth

CI-safe only — `apid` + Postgres, no `//go:build metal`. The
existing scan_project_e2e_test.go covers the plan wire
contract; the new files cover the apply wire contract:

| File | Cases | Focus |
|---|---|---|
| `apply_project_e2e_test.go` | 8 | happy path, project row, wire shape, ScanDir leak, kind parity |
| `apply_project_builds_e2e_test.go` | 10 | the new behaviour: build/deployment per workload |
| `apply_project_quota_e2e_test.go` | 10 | plan × app cap + cron cap; zero rows on over-cap |
| `apply_project_guards_e2e_test.go` | 8 | never-empty, production-branch, scan-source downgrade |
| `apply_project_diff_e2e_test.go` | 7 | 2nd apply: added/changed/removed/unchanged + cascades |
| `apply_project_inputs_e2e_test.go` | 17 | auth/scope/MFA, plan_token, idempotency, malicious tarballs, managed, audit taxonomy |

## Consequences

Positive:

- `gregale deploy` on a multi-workload repo now produces
  builds the customer can wait on or stream logs from.
- The CLI parity trap (single-app works, multi-app silently
  parks) is closed.
- The ScanDir leak is fixed — every apply cleans up.
- Three callsites share one helper; future build-enqueue
  changes (e.g., a new build kind) land in one place.
- ~60 e2e cases pin the apply path so this class of gap
  cannot recur silently.

Negative:

- The `Builds` field is `omitempty` but the SDK and CLI both
  decode it; a future change that *removes* a field from
  `AppliedBuild` will need a coordinated SDK regen.
- Per-workload tarballs under `<spoolRoot>/projects/...` are
  not reaped — githubd's are keyed on commit SHA and
  overwritten in place, which doesn't map to apply. Filed as
  follow-up.
- Three files (`deploy_inputs.go`, `githubd_bridge.go`,
  `scan_service.go`) now route through `pkg/apid/apidsource`;
  any future change to the build-enqueue contract must touch
  the helper, not the callers.

## References

- ADR-050 — repo decomposition and project object.
- ADR-005 — cold boot must always work (snapshots are cache).
- ADR-003 — builds run inside ephemeral builder microVMs.
- `pkg/apid/apidsource/apidsource.go` — shared helper.
- `pkg/githubd/staging.go::RepackageRootTree` — generic
  per-workload tarball writer.
- `cmd/apid/scan_service.go::applyBuildsForAddedChanged` — the
  new apply-time enqueue loop.
- `cmd/e2e/apply_project_*_e2e_test.go` — six test files.

# ADR-052 · Adding a function runtime

- **Status:** accepted
- **Date:** 2026-07-31
- **Decision:** A new function runtime is a 7-layer additive change
  distributed across migrations, schema, apid whitelist, runner shim,
  imaged handler surfaces, base Dockerfile + auto-stage wiring, and
  CLI templates. The touch-list below is canonical; the order is
  conventional. Every new runtime ships via 2-3 PRs (runtime matrix
  in PR 1, base auto-stage in PR 2 if it didn't pre-exist, document
  surface in PR 3 if the procedure itself is changing). No
  default-flip is performed in the same PR — that's a separate
  explicit ADR. Adding a *new version* of an existing runtime
  (`node22 → node24`, `python312 → python313`) is the worked example
  for this ADR; adding a *new language* (`ruby33`) is the
  generalization — the same 7-layer table, the same matrix tests,
  the same per-arch partition via `pkg/sched/BaseKeyForArch`.

## Decision

| Layer | Surface | Files | Pin test |
|---|---|---|---|
| 1 | DB enum acceptance | `migrations/0007N_*.sql` (DROP-and-re-ADD `apps_runtime_check`), paired `*_test.go` (round-trip + older-runtimes preservation + bogus-reject + contiguity), `schema.sql:355` | `TestMigrations_0007N_AppRuntime*`, `migrations/embed_test.go::TestMigrationsContiguous` |
| 2 | apid server-side validation | `cmd/apid/handlers.go::buildApp` allow-list + error message; `pkg/api/dto.go` + `pkg/state/types.go` comment-only updates; `pkg/apid/openapi.yaml` (3 enum sites) | `TestCreateApp_Function<New>Runtime`, `TestCreateApp_Function<New>BadRuntime`, `make spec-check` |
| 3 | Runner shim (per-request subprocess) | `guest/runners/<new>/main.go` + `main_test.go`; `Makefile::GUEST_RUNNERS` (incl. comment); `deploy/lima/faas-metal.yaml` runner build loop + per-arch symlinks | `Test<New>RunnerHandlerDefault` pins `/app/<new>.js` or `/app/handler.py` against imaged argv |
| 4 | imaged function-layer matrix | `pkg/imaged/base.go` (new `BaseRef<X>` + `Runtime<X>` + `baseRefFor` case); `pkg/imaged/handler.go` 6 surfaces: path fields (64-83), setters (138-178), allow-list (buildFunctionLayer 680), argv branches (manifest.Entrypoint override blocks 718-728), `runnerPathFor` cases (825-837), `runtimeToEnvSuffix` cases (842-854); `cmd/imaged/main.go::main` env-var wiring loop | `TestBaseRefFor_Runtimes`, `TestRunnerPathFor_Runtimes`, `TestRuntimeToEnvSuffix_Runtimes`, `TestBuildFunctionLayer_Runtimes`, `TestBuildFunctionLayer_MissingRunnerFailsLoud` |
| 5 | Base image selection + auto-stage | `pkg/imaged/base_stage.go` (new `RuntimeBaseRef` row in `DefaultRuntimeBaseRefs`); `cmd/imaged/main.go` EnsureBases call (already iterates the table); `images/runner-<new>.Dockerfile` + `images/Dockerfile.lock` entry; `images/README.md` roster; `deploy/digitalocean/sealed.env.example` (commented-out `FAAS_DEPLOY_BASE_REF_<RUNTIME>`); `deploy/lima/faas-metal.yaml` symlinks | `TestEnsureBases_AllRowsStage`, `TestEnsureBases_OperatorOverride_DigestPinnedWins`, `TestEnsureBases_OperatorOverride_TagOnlyFailsLoud`, `TestEnsureBases_SkipsOnDigestMatch`, `TestDefaultRuntimeBaseRefs_HasExpectedRuntimes`, `make images-lock-check` |
| 6 | Tests + docs | `docs/runtimes/<new>.md` (mirrors `docs/runtimes/go124.md` template — opener, function contract, minimal handler, local smoke test, app contract, base image, operator-staging, detection priority, failure modes, see also); `docs/STATUS.md` roster; `docs/faas_implementation_spec.md` lines 69, 119, 163, 165, 197, 201 (forward-references this ADR), 232, 632, 644, 731 | `make spec-check` (vacuum lint); markdown review per `docs/runtimes/*.md` template |
| 7 | CLI templates | `cmd/gregale/templates/function-<new>/` (handler file + package.json/requirements.txt + README); `cmd/gregale/templates/embed.go` (recompile via `make templates`); `cmd/gregale/commands2.go` switch case for the new template (forces runtime + handler); `cmd/gregale/commands2.go` help string widening | `gregale deploy --template function-<new>` round-trip on Lima |

## Consequences

### Positive

- **Future runtimes are a 1-PR append** per layer. The grep test is
  "find every switch over the runtime id, add a case, add a test
  row". Future bumps (`node26`, `python314`) collapse to
  `~migrations + schema + runner shim + imaged matrix row + base
  Dockerfile + Gregale template + per-runtime doc` — mostly 6-12
  hundred-line edits. The drift detector at
  `TestDefaultRuntimeBaseRefs_HasExpectedRuntimes` ensures the
  table can't fall behind the runtime enum.
- **Default-flip is a separate explicit ADR** — per the
  `go124-alpine` precedent in `docs/runtimes/go124.md`. The
  procedure is intentionally orthogonal so future "default to node24"
  can be measured against `snapshot_fleet_avg_mb` (already an alert
  in `pkg/api/limits.go::FleetSnapshotAvgTargetMB = 130`, alarm 160)
  without re-architecting the runtime-add path.
- **Path-versioned handler filenames** (`/app/node24.js`,
  `/app/node22.js`) are coordinated with imaged's argv matrix, so a
  rename at the runner shim is caught at unit-test time, not on first
  wake (the precedent: the early node22 PR shipped with
  `/app/handler.js` and silently mismatched imaged's argv, only
  surfacing as a first-wake rollback). The version-neutral Python
  filename (`/app/handler.py`) is unchanged because the version is
  bound by the OCI base, not by the handler filename.
- **Operator-side digest-pinned overrides** (`FAAS_DEPLOY_BASE_REF_<RUNTIME>`)
  follow the same posture the deploy-time `FAAS_DEPLOY_BASE_REF`
  has: tag-only overrides abort imaged startup loud with a
  one-line error naming the offending env var. The auto-stage loop
  re-uses the existing `oci.ParseReference.Digest` gate, so an
  operator who wants to pin prod runs `make images-lock-update`,
  publishes the digest, sets the env var, and forgets about it.
- **Spec drift becomes a PR-time diagnostic**, not a quarterly
  audit. The load-bearing false claim at
  `docs/faas_implementation_spec.md:201` ("Adding a runtime = adding
  one runner image + one detection rule") is rewritten by this ADR;
  the spec is now a redirect, not a recipe.

### Negative

- **The table is duplicated knowledge.** The cross-runtime
  consistency pins are `migrations/embed_test.go::TestMigrationsContiguous`,
  `TestBaseRefFor_Runtimes`, `TestRunnerPathFor_Runtimes`,
  `TestBuildFunctionLayer_Runtimes`, and
  `TestDefaultRuntimeBaseRefs_HasExpectedRuntimes`. A drift
  detector that asserts "every switch over the runtime id has a
  corresponding row in `DefaultRuntimeBaseRefs`" is the obvious
  next step; it's out of scope here — review is fine for the
  current row count (6; ~20 is when review breaks down, per the
  `pkg/api/limits.go` constant naming convention).
- **Default-flip is a future-ADR cost.** `node22` stays the default
  for new function apps. The default-flip is intentionally not in
  this PR; measuring `snapshot_fleet_avg_mb` with both bases
  co-resident is the prerequisite. The 4-byte cost of the extra
  runtime row in `DefaultRuntimeBaseRefs` is acceptable; the
  two-drive scheme amortizes drive0 across every parked app.
- **Tier 1's 3-PR split is lockstep-dependent.** PR 1 widens the
  enum and wired matrix; PR 2 wires the auto-stage loop (inherits
  PR 1's matrix); PR 3 documents the procedure (inherits both). The
  worked example this PR documents is the Tier 1 implementation.
  Cross-PR slot collisions are mitigated by `ADR-041`'s
  reservation convention; the Tier 1 slots 00075 (widening) and
  DR-052 slot reservation are not yet needed because the Tier 1
  migrations are bounded (just one widening).
- **3 runtime allow-lists must widen in lockstep** (DB CHECK, apid
  whitelist, imaged runtime matrix). The matrix tests at
  `pkg/imaged/handler_test.go:1090, 1117, 1148, 1184, 1291` are
  the load-bearing pins — a future contributor who widens the DB
  CHECK without widening the apid whitelist (or vice versa) trips
  a unit test at PR time, not a runtime error in production.

### Rejected alternatives

- **Filename-neutral handlers** (`/app/handler.js`,
  `/app/handler.py`). Rejected: versioned filenames
  (`/app/node24.js`, `/app/node22.js`) catch a future default-flip
  migration as a co-deployable textual replacement across parked
  apps' rootfs, instead of forcing a runtime-versioned dispatch
  table inside the runner shim. Python handlers stay
  version-neutral because their interpretation surface is the
  interpreter binary's version (bound by the OCI base), not the
  `.py` filename.
- **Adding a `runtime_version` column to `BuildManifest`.**
  Rejected: the runtime version is bound by the OCI base ref
  (digest-pinned via `FAAS_DEPLOY_BASE_REF_<RUNTIME>`), which is
  already on `apps`. Adding a second version-bearing column
  creates a redundancy and a class of bugs where the two diverge.
  The base-ref path also covers future `node24-alpine` etc.
  without further schema work.
- **Auto-staging runtime bases by reading `apps.runtime` rows at
  startup.** Rejected: a 130 MB pull races the customer's first
  wake. The Tier 1 PR 2 design stages every row in
  `DefaultRuntimeBaseRefs` at startup; the customer wake wire
  reads `/srv/fc/base/runner-<runtime>-<arch>.ext4` from
  `pkg/sched/BaseKeyForArch`, the same path used by the
  builder-base row. The pull cost is a startup cost, not a
  per-wake cost.
- **Generating the 7-layer partial-add from a single template.**
  Rejected: the layers touch Go code, SQL migrations, OCI
  references, `embed.go` files, and `mkfs.ext4` recipes — none of
  which share a uniform input format. A template would re-create
  the matrix-test failures it's supposed to prevent. The procedure
  table is the unit of generation; the human follows it.
- **Per-runtime subcommands on apid** (a separate
  `POST /v1/apps/{id}/deployments?runtime=node24` toggle). Rejected:
  the runtime is a property of the function app, not of the
  deployment, and re-deploying to switch runtime is the same wire
  path as the first deploy. The runtime lives on `apps.runtime`;
  the deployment carries the runtime implicitly via the app row.

## Cross-references

- Forced by PR #1 of Tier 1 (`node24` + `python313` support):
  `migrations/00075_app_runtime_node24_python313.sql`,
  `pkg/imaged/base.go::baseRefFor` widening,
  `pkg/imaged/handler_test.go` matrix rows,
  `docs/runtimes/{node24,python313}.md` per-runtime docs.
- Forced by PR #2 of Tier 1: `pkg/imaged/base_stage.go::EnsureBases`
  + `DefaultRuntimeBaseRefs` table,
  `images/runner-{node24,python313}.Dockerfile`,
  `images/Dockerfile.lock` entries,
  `deploy/digitalocean/sealed.env.example::FAAS_DEPLOY_BASE_REF_*`.
- Loading constraint:
  `migrations/00001_init.sql:35` (initial `apps_runtime_check`
  literal), `cmd/apid/handlers.go:101-104` (apid allow-list),
  `pkg/apid/openapi.yaml` (3 enum sites).
- Spec: §4.5/§4.6/§4.9 (build pipeline + two-drive scheme + §4.9
  envelope contract), §6 (state machine — runtime is on `apps`
  not on `instances`), §14 (M7 milestone lists function runtimes
  one final time — ADR-052 re-baselines the list).
- Prior runtimes: `go124-alpine` shipped in PR #373 (Tier 2) — the
  worked example for adding a sibling variant of an existing
  runtime.
- Cross-ADR: ADR-005 (cold boot always works — the auto-stage
  loop's idempotency is a corollary), ADR-009 (identical inner
  network world — the runtime choice doesn't change the inner
  network), ADR-038 (build attestation — base images are content-
  addressed, the OCI layer order is what the build emits), ADR-041
  (migration slot reservation convention — Tier 1 used slots
  00075, future runtime additions reserve at PR-open per this ADR).

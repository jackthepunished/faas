# ADR-088 · Centralize source-marker detection in `pkg/markers` (issue #736 / DEPLOY-PROV-2)

- **Status:** accepted
- **Date:** 2026-08-09
- **Issue:** #736 / DEPLOY-PROV-2
- **Supersedes:** ADR-087's "kept in sync intentionally" trade-off for the CLI version-parser mirror; the open-set paragraph in `cmd/gregale/pack.go:18-25` ("importing pkg/builderd would pull the entire server stack into the CLI binary; the two copies are the accepted trade for zero server blast radius") is now obsolete — the CLI imports `pkg/markers` directly, not `pkg/builderd`.
- **Related:** ADR-086 (depth-2 nested marker hint), ADR-087 (per-framework version priority), ADR-083 (function-vs-app shape). `pkg/builderd/detect.go:41-95` (the server-side detector this replaces). `cmd/gregale/pack.go:118-126` (the CLI's `appMarker` map and the inline switch in `detectShape` at `pack.go:622-628` — both superseded).

## Context

The same top-level marker switch (Dockerfile / package.json / requirements.txt / pyproject.toml / Pipfile / setup.py / go.mod) is implemented **three times** today:

1. **`pkg/builderd/detect.go:41-95`** — server-authoritative. Walks the top-level entries of the apid-spooled tarball and picks a `Framework`. Pure stdlib.
2. **`cmd/gregale/pack.go:118-126` (`appMarker`) + `pack.go:154-191` (`detectFramework`)** — CLI mirror. `os.ReadDir(srcDir)` + map lookup + Dockerfile-wins post-pass. The block comment at `pack.go:18-25` explicitly states: *"importing pkg/builderd would pull the entire server stack (DB, scheduler, firecracker) into the CLI binary. The rule is small and stable; the two copies are the accepted trade for zero server blast radius."*
3. **`cmd/gregale/pack.go:625-626` (inline switch in `detectShape`)** — the THIRD copy. Lists the same markers as `appMarker`, but as a `case` chain.

The duplication is silent drift waiting to happen:

- A new marker added to one side produces a friendly local progress message while the build still fails with `FrameworkUnknown`.
- Removing a marker on the server leaves the CLI telling users their repo is supported when it isn't.
- The version parser has a parallel duplication: `pkg/builderd/detectversion.go` (server) vs `cmd/gregale/pack.go::detectFrameworkVersion` (CLI), with all the regex literals mirrored. ADR-087 noted this duplication but accepted it as the price of "the CLI banner must render before the multipart POST."

Today there are also TWO `Framework`-shaped types: `pkg/builderd.Framework` (server) and `cmd/gregale.framework` (CLI), with the same string values but no shared identity.

The CLI cannot import `pkg/builderd` directly — `pkg/builderd/detect.go` is stdlib-only, but the package as a whole transitively imports `vm_metal.go`, `dispatch.go`, `reaper.go`, `slot.go`, and the Firecracker server stack. Pulling all of that into `cmd/gregale` bloats the CLI binary by ~50 MB and adds DB / scheduler dependencies the CLI doesn't need.

## Decision

Extract a new top-level `pkg/markers` package containing ONLY the marker switch + version parser. `pkg/builderd` re-exports the type via alias. `cmd/gregale` imports `pkg/markers` directly.

### Sub-decisions

1. **Single source of truth = `pkg/markers`.** Both `pkg/builderd` (server) and `cmd/gregale` (CLI) import this package. The CLI's local `framework` type becomes a 1-line type alias (`type framework = markers.Framework`) to minimize the diff to every `fw == fwUnknown` call site; the 5 constants `fwNode`/`fwPython`/`fwGo`/`fwDocker`/`fwUnknown` similarly become `const ( fwNode = markers.FrameworkNode; ... )`.
2. **CLI never imports `pkg/builderd`.** Preserves the binary-size boundary. `pkg/builderd` re-exports `Framework` and the 5 constants via type alias, so existing server-side callers (`pkg/builderd/builderd.go:339`, `build_base.go`, `cache.go`) compile unchanged.
3. **Marker list is a priority-ordered slice, not a map.** Go's map iteration is non-deterministic; the slice's order IS the priority order. `Dockerfile` first (wins over `package.json` / `go.mod`), then `package.json` (Node), then the four Python markers, then `go.mod`. Adding a new marker is one line.
4. **Version parser moves with the marker switch.** The CLI's `cmd/gregale/pack.go::detectFrameworkVersion` mirror (~95 lines) is deleted; the CLI calls `markers.VersionFromFS(os.DirFS(srcDir), fw)` directly. ADR-087's "kept in sync intentionally" comments are obsolete.
5. **Function handler files stay CLI-only.** `functionHandlerFiles` (`cmd/gregale/pack.go:80-85`) is a shape concern (handler.* → function mode), not a marker. Server doesn't care which file is the handler.
6. **`fs.FS` is the canonical input.** Both `DetectFromFS(fsys)` and `VersionFromFS(fsys, fw)` use `io/fs`. `DetectFromTarball(path)` / `VersionFromTarball(path, fw)` are server-side shims that open the tarball and either walk entries or extract a single file. The CLI no longer needs an `os.ReadDir`-based mirror — `os.DirFS(srcDir)` is the one-liner.
7. **Parity test is the load-bearing acceptance gate.** `pkg/markers/parity_test.go::TestDetectCLIParity` runs the same fixture (CLI dir + gzipped tarball of the same contents) through both code paths and asserts identical `Framework`. The 23-fixture table covers all marker permutations + nested-ignored + handler-alone + case-insensitive + dockerfile-priority pins. A regression in the priority order, the case-folding, or the nested-ignored rule on either side flips exactly one subtest.
8. **Both sides return `(FrameworkUnknown, nil)` for missing markers.** The original `pkg/builderd.Detect` returned an error on miss; the parity test pins the CLI shape as authoritative because the CLI's existing `detectFramework` returned `fwUnknown` without an error. The `Detector` shim in `pkg/builderd` wraps the (unknown, nil) tuple into an error so the build pipeline at `builderd.go:339` can record a `user_error` failure_class (see `TestProcessOne_FrameworkDetectFailsFlipsDeployment` and `TestProcessOne_UnknownFrameworkFails`).
9. **64 KB cap per version-marker file.** `pkg/markers/detectversion.go::maxVersionFileBytes = 64 * 1024` — refused-oversize defends against the OOM-by-source attack. Applies to both FS (`readFSFile`) and tarball (`readTarballFile`) paths.
10. **Rollback is three-commits-revertible.** The pre-refactor state is three files: `pkg/builderd/detect.go`, `pkg/builderd/detectversion.go`, and the duplicated block in `cmd/gregale/pack.go` (the `appMarker` map + `detectFramework` body + `detectFrameworkVersion` + 8 cli* helpers + the inline switch in `detectShape`). The post-refactor state is three commits: (1) add `pkg/markers`, (2) shim `pkg/builderd`, (3) refactor `cmd/gregale`. Symmetric, so `git revert` is the rollback procedure. No DB / wire / behaviour change.

### Package layout

```
pkg/markers/
├── framework.go          # Framework type + 5 constants
├── markers.go            # appMarkers slice + MarkerFor / IsAppMarker / Markers
├── detect.go             # DetectFromFS / DetectFromTarball
├── detectversion.go      # VersionFromFS / VersionFromTarball + parsers
├── detect_test.go        # 21 cases (5 tarball priority pins, 10 fs path pins, 5 marker lookup pins, 1 fixture-table exhaustive)
├── detectversion_test.go # 28 cases (18 server-side ports, 10 fs path pins)
└── parity_test.go        # 23-case TestDetectCLIParity + 7-case TestVersionParity
```

`pkg/markers` imports only stdlib (`archive/tar`, `compress/gzip`, `encoding/json`, `errors`, `fmt`, `io`, `io/fs`, `os`, `regexp`, `strings`). No transitive deps on `pkg/builderd`, `pkg/sched`, `pkg/state`, `pkg/db`, `pkg/wire`, `pkg/events`.

### File-by-file change list

| File | Change |
|------|--------|
| `pkg/markers/framework.go` | NEW. `Framework` type + constants. |
| `pkg/markers/markers.go` | NEW. `appMarkers` slice + `MarkerFor` / `IsAppMarker` / `Markers`. |
| `pkg/markers/detect.go` | NEW. `DetectFromFS` / `DetectFromTarball`. |
| `pkg/markers/detectversion.go` | NEW. `VersionFromFS` / `VersionFromTarball` + per-parser funcs (moved verbatim from `pkg/builderd/detectversion.go`). |
| `pkg/markers/detect_test.go` | NEW. Port of `pkg/builderd/detect_test.go` + `DetectFromFS` tests. |
| `pkg/markers/detectversion_test.go` | NEW. Port of `pkg/builderd/detectversion_test.go` + `VersionFromFS` tests. |
| `pkg/markers/parity_test.go` | NEW. `TestDetectCLIParity` + `TestVersionParity` cross-fixture tables. |
| `pkg/builderd/detect.go` | REWRITTEN. ~50-line shim with `type Framework = markers.Framework` + `Detect` / `DetectFromFS` / `DetectWithVersion` methods. |
| `pkg/builderd/detect_test.go` | DELETED (moved to `pkg/markers`). |
| `pkg/builderd/detectversion.go` | DELETED (moved to `pkg/markers`). |
| `pkg/builderd/detectversion_test.go` | DELETED (moved to `pkg/markers`). |
| `pkg/builderd/builderd.go` | UNCHANGED. Type alias keeps the existing `b.detector.DetectWithVersion` call site compiling. |
| `pkg/builderd/builderd_test.go` | `makeTarballWithName` inlined (was a wrapper around the deleted `makeTarball` helper). |
| `cmd/gregale/pack.go` | EDITED. Drop `appMarker` map, `detectFramework` body, `detectShape` inline switch, `detectFrameworkVersion`, 8 cli* helpers (`cliReadFile`, `cliReadFirstLine`, `stripVersionPrefix`, `normalizeVersion`, `versionLikeCLI`, `cliVersionFromPackageJSONNode`, `cliVersionFromPyprojectRequires`, `cliVersionFromGoModDirective`). Add `import "github.com/onebox-faas/faas/pkg/markers"`. `framework` type becomes alias. `detectFramework` and `detectFrameworkVersion` are kept as 1-line wrappers for `pack_test.go` test-compat. |
| `cmd/gregale/pack_test.go` | UNCHANGED. `fwNode`/`fwPython`/`fwUnknown` constants + `detectFramework`/`detectFrameworkVersion` wrappers keep ~30 test cases compiling. |
| `cmd/gregale/deploy_shape_e2e_test.go` | UNCHANGED. Banner format is byte-identical. |

## Consequences

### Positive

- One source of truth for the marker switch. Adding a marker (e.g. Cargo.toml for Rust) is one line in `pkg/markers.appMarkers`; both CLI and server pick it up automatically.
- One source of truth for the version parser. The CLI's 95-line mirror is gone; the regex literals live in one place.
- The CLI binary doesn't grow. `pkg/markers` is stdlib-only; the import is a few KB. Verified: `ls -lh /tmp/gregale` before vs. after the refactor is byte-equal (within ±1 KB).
- The parity test pins both sides against future divergence. A regression in the priority order, case-folding, or nested-ignored rule on either side flips exactly one subtest.
- The CLI now imports `pkg/markers` (a fresh, lean package) rather than relying on a comment-block warning about `pkg/builderd`.

### Negative

- `pkg/builderd` becomes a "shim that re-exports a single type"; the comment at `pkg/builderd/detect.go:13` documents this as intentional.
- `pkg/builderd.Detect` returns an error on unknown markers (preserving the original `builderd.go:339` failure-class contract); `markers.DetectFromTarball` returns `(unknown, nil)`. The shim wraps the asymmetry. Documented in `pkg/builderd/detect.go::Detect`.
- The `framework` keyword in `cmd/gregale/pack.go` is now a type alias. A future engineer who tries to "fix" the alias by re-introducing the local `type framework string` would break compilation against `markers.Framework` — caught immediately.

### Out of scope (not addressed here)

- `pkg/reposcan/workspaces.go:259-278` (`hasMarker`) has an extended marker set that includes `Cargo.toml` and `pom.xml` — extras that the framework auto-detect doesn't yet support. The ADR mentions this for follow-up; the Rust/Java workspace detection is a separate ADR.
- `inferFunctionRuntime` (CLI-only) and `functionHandlerFiles` are CLI-only shape concerns, not source markers. They stay in `cmd/gregale/pack.go`.
- The build pipeline's `user_error` vs `infra` classification (already coded at `pkg/builderd/builderd.go:341`) is unchanged.

## Rollback

Revert the three commits in this PR. The pre-refactor state is restored: `pkg/builderd/detect.go` + `detectversion.go` come back, `cmd/gregale/pack.go` reverts to the duplicated switch + mirror parser, `pkg/markers/` is deleted. No DB / wire / behaviour change.

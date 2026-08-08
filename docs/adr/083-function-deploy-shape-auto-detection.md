# ADR-083 · Function-vs-app shape auto-detection in `gregale deploy` (issue #737 / DEPLOY-PROV-3)

- **Status:** accepted
- **Date:** 2026-08-08
- **Issue:** #737 / DEPLOY-PROV-3
- **Supersedes:** none
- **Related:** docs/faas_implementation_spec.md §4.9 (function runner invariants); ADR-042 (`pkg/appmetrics`); issue #735 (full-stack deploy→wake acceptance test)

## Context

Today `gregale deploy` (no source flags) is implicitly an **application**
deploy. The CLI packs the current directory via `autoPackCwd`
(`cmd/gregale/pack.go:283`) and never carries `runtime` / `handler` on
the wire, so the server's builderd+imaged path runs Railpack against
whatever framework is at the tarball root. A *function* deploy —
runtime + handler, single-request invocations — requires explicit
`--runtime` and `--handler` flags. The escape hatch is `gregale
--template function-node` (or one of the four `function-*` siblings),
which forces those flags and tars the embedded starter project
(`cmd/gregale/commands2.go:594-625`).

This is a discoverability cliff called out by issue #737:

1. A user with a single `handler.js` at the project root gets the
   no-source error at `cmd/gregale/commands2.go:666-668`, which lists
   `package.json / requirements.txt / … / Dockerfile` and never
   mentions "function".
2. A user with `package.json` (the vast majority of Node projects)
   gets an app-shaped Railpack deploy — even if they intended a
   single handler function. The CLI says nothing about the function
   shape they could have chosen.
3. The Tier-2 launch readiness criterion is "the first five minutes of
   a customer's first session should not require reading the spec."
   Silent wrong-shape deploys fail that bar.

A fix has two halves: detect function-shaped repos from the cwd, and
print the detected shape before the multipart upload so the customer
sees what `gregale` decided.

## Decision

### 1. Auto-pick function shape on `handler.*`-only dirs

`gregale deploy` (no source flags) on a directory whose top-level
entries contain exactly one of `handler.js`, `handler.ts`,
`handler.py`, `handler.go` AND none of `package.json`,
`requirements.txt`, `pyproject.toml`, `Pipfile`, `setup.py`, `go.mod`,
`Dockerfile` is a **function** deploy. A `README.md` and dotfiles are
ignored (most repos have them).

The runtime is inferred from the file extension:

| extension | runtime  |
|-----------|----------|
| `.js`     | `node22` |
| `.ts`     | `node22` |
| `.py`     | `python312` |
| `.go`     | `go124` |

The wire `handler` field is the literal `handler.handler` value,
mirroring `defaultTemplateHandler` at `cmd/gregale/commands2.go:48`
(what the embedded `function-*` templates force today). imaged's
function-layer manifest already rewrites this to `/app/<runtime>.js`
(etc.) per the §4.9 function runner contract.

### 2. App markers always win

If any app marker is present at the cwd root, the shape is **app**,
even if `handler.{js,py,go}` is also present. The function convention
is "single handler file, nothing else"; a `package.json` next to a
`handler.js` is unambiguously a Node project, and the customer can
explicitly force function mode with `--function` if they really mean
it. Auto-detection never overrules an explicit shape flag.

### 3. CLI prints the detected shape BEFORE the POST

Every successful `gregale deploy` (after auto-detection or explicit
shape, before the multipart upload) prints one of:

```
Detected: function, runtime=node22, handler=handler.handler
```

or

```
Detected: app, framework=node-railpack
```

The customer's first response from the CLI is the deploy shape. This
is the customer-visible acceptance gate for issue #737 and is pinned
by `cmd/gregale/deploy_shape_e2e_test.go` (no metal tag; runs in CI).

### 4. Explicit override flags

Two new flags force the shape:

- `--function` — skip detection, force function mode. Requires
  `--runtime`. If `--handler` is unset, default to `handler.handler`
  (matching the convention).
- `--app` — skip detection, force app mode. Clears any `--runtime` /
  `--handler` the customer also set with a `WARN:` line. Silently
  mixing is exactly the bug this ADR is fixing.

`--function` and `--app` are mutually exclusive. Detection is the
default whenever neither is set.

### 5. The wire surface stays unchanged

`pkg/api/dto.go:CreateAppRequest` already carries `Type` ("app" |
"function") and `Runtime`. `pkg/api/multipart.go:8-29` already writes
`runtime` and `handler` form fields when non-empty. apid's
`buildApp` validator at `cmd/apid/handlers.go:89-100` already accepts
`Type=app|function` and validates the function runtime. **No schema
migration.** No `deployments` table change.

The CLI's auto-detection path sets `runtime`/`handler` on the
multipart form and `Type="function"` on the `CreateApp` call. The
existing apid path handles it.

### 6. Detection lives in the CLI, not on the server

The server's `pkg/builderd/detect.go:41-95` keeps its current
top-level-sniff contract — a source tarball is always built into an
app image. The CLI's detection runs on the customer's cwd BEFORE the
tarball is uploaded; it determines the multipart form fields and the
`CreateApp.Type`, not what builderd does. This preserves the existing
seam where the server is authoritative on framework detection from
the tarball and the CLI is authoritative on what the customer meant.

### 7. Acceptance gate

`go test ./cmd/gregale/ -run TestDeployShapeE2E -v` packs a tmpdir
containing only `handler.js`, invokes the CLI subprocess, captures
stdout, and asserts `Detected: function, runtime=node22,
handler=handler.handler` appears BEFORE the multipart upload. A
regression that drops the print line, picks the wrong shape, or moves
the print line after the POST fails the gate.

The metal-tagged 9th subtest of
`cmd/e2e/source_deploy_wake_metal_test.go`
(`auto-pick-function-from-handler-js`) drives the full chain — function
deploy → live → wake → handler invocation. It exercises the wire
contract end-to-end and pins the apid function-deploy path.

## Consequences

### Positive

- A customer with a single `handler.js` no longer hits the no-source
  error. The first command they run does what they meant.
- Every `gregale deploy` (success path) names the deploy shape. The
  "what shape did the CLI decide?" question has a customer-visible
  answer.
- The function convention is "single handler file at the root, no
  app markers" — discoverable from a `ls` of the project root. No
  new config-file convention to learn.

### Negative

- A `handler.js` lurking next to a real `package.json` will be
  ignored — the customer must explicitly pass `--function` to force
  it. We accept this: "single handler, nothing else" is the function
  convention by definition, and silently making `package.json +
  handler.js` a function would surprise every existing Node user.
- The detection rule is a snapshot of the file root at deploy time;
  later edits to add `package.json` will silently re-shape the next
  deploy. We accept this; the print line is the customer-visible
  signal, and `ls` answers the question for them.
- The runtime whitelist stays at the current 6 values
  (`node22 / node24 / python312 / python313 / go124 / go124-alpine`).
  Adding `node24` / `python313` to the auto-detection map is a
  follow-up ADR (one runtime per Tier-1 PR row; the same shape the
  embedded templates already take).

### Neutral

- `cmd/gregale/pack.go` gains two helpers (`detectShape`,
  `inferFunctionRuntime`) and two unit-test cases for each. The
  server-side `pkg/builderd/detect.go` is untouched.
- `pkg/api/client.go:62-68` `DeployTarball` gains an optional `Type`
  parameter; existing callers default to `"app"` so no behaviour
  change.
- The CLI's `cmdDeployTarball` carries two new flags (`--function`,
  `--app`); the flag parsing shape matches the existing
  `--require-authn` / `--no-require-authn` pair (mutex check,
  `fs.Visit` for explicit-vs-unset).
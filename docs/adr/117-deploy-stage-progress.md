# ADR-117 · Deploy stage progress (typed `event: stage` SSE frame + CLI ticker)

- **Status:** **Proposed**
- **Date:** 2026-08-18
- **Decision:** The `/v1/deployments/{id}/logs` SSE stream
  publishes a typed `event: stage` frame for each named pipeline
  stage the customer's deploy passes through. The closed 6-stage
  vocabulary is owned by `pkg/state.StageName` and is the canonical
  surface for any future ticker UI (CLI today, dashboard tomorrow).
  The CLI renders the stream as a live ticker (TTY-gated ANSI
  cursor-up redraw) with a static fallback for pipes / `--json` /
  `NO_COLOR`. The deploy UX moves from "single spinner" to
  "6-row named progress block with per-stage elapsed time" — the
  same affordance Render's deploy log exposes.

## Context

`gregale deploy` today prints one `→ build queued …` line, then
streams raw builderd stdout via `event: log`, and ends with
`✓ Deployed. …`. The deployment pipeline already has clean
internal stage boundaries (`pending → building → imaging →
snapshotting → live | failed | superseded` on `deployments.status`)
but the wire surface exposes them only as one coarse field — the
CLI cannot render per-stage elapsed timing. Customers want the
same confidence-building narrative that Render's deploy log gives:
a list of named stages with the time each one took, prominent in
the deploy output.

This is the same shape customers have come to expect from every
PaaS deploy (Render, Fly, Heroku, Vercel). Without it Gregale's
deploy UX looks unfinished next to those platforms.

## Decision

### Wire vocabulary

A single typed SSE frame carries the per-stage progress:

```
event: stage
data: {"name":"<StageName>","started_at":"<RFC3339Nano>","duration_ms":<int64>,"status":"in_progress"|"completed"|"failed"[,"reason":"<string>"]}
```

- `name` ∈ closed set of 6 stages (see A1 below).
- `status:"in_progress"` emitted once per stage on entry.
- `status:"completed"` emitted once per stage on exit (with
  `duration_ms` measured server-side).
- `status:"failed"` emitted once for the active stage on
  `DeployFailed`, carrying the optional `reason` from
  `deployments.failure_reason`.

The frame is additive — every existing SSE consumer that doesn't
decode `event: stage` is unaffected. The `pkg/api/sse.go` decoder
already preserves `event:` names verbatim, so SDK consumers don't
need a regen.

A1 — closed 6-stage vocabulary:

| StageName           | Human label          | Server-side transition sites                                       |
|---------------------|----------------------|--------------------------------------------------------------------|
| `source_download`   | Source downloaded    | imaged `handleDeploySourceChanged` Pending→Building               |
| `dependency_restore`| Dependencies restored| imaged `buildImageLayer` / `buildFunctionLayer` Building→Imaging  |
| `image_build`       | Image built          | imaged `handleDeploySourceChanged` Imaging→Snapshotting           |
| `security_scan`     | Security scan        | imaged `buildImageLayer` scan-completion stamp                    |
| `snapshot_prepare`  | Snapshot prepared    | imaged `handleSnapshotBoot` Imaging→Snapshotting                  |
| `readiness`         | Readiness passed     | imaged `MarkDeploymentLive` snapshot_prepare→readiness close       |

The customer-visible set omits builderd-internal micro-states
(`imaging` / `snapshotting` are one user-visible "image build"
step) and omits the terminal `Deployment live` row since
`✓ Deployed. …` already conveys it.

### Schema

`deployments.stage_state jsonb NOT NULL DEFAULT
'{"current":"source_download","current_started_at":null,"history":[]}'::jsonb`
with a CHECK constraint enforcing `current ∈ {source_download,
dependency_restore, image_build, security_scan, snapshot_prepare,
readiness}`. The column is owned entirely by the new
`pkg/state.Store.AppendDeploymentStage` — handlers never write it
directly. The closed enum is enforced at the database layer so a
rogue writer can't insert an out-of-vocabulary stage.

Migration: `migrations/00300_deployments_stage_state.sql`
(append-only; mirrors the `00286` / `00287` jsonb+CHECK pattern).
Initial draft used slot `00296` with the cross-PR slot fence
pattern; the slot was renumbered to `00300` post-review because
main `b3d4cf7c` carries a `00296_reserve_slot.sql` reservation
fence for PR #986 (ADR-120 domain doctor) — see
[[cross-pr-slot-precheck-pr-867-collision-2026-08-13]].

### AppendDeploymentStage semantics

```go
func (s *Store) AppendDeploymentStage(ctx, id, from, to StageName, at time.Time, reason string) (Deployment, error)
```

Three cases:

1. **Forward transition** (`from != to`): append a completed row
   for `from` to `history` (with `DurationMs`), then flip
   `current = to` + `current_started_at = at`.
2. **Failure stamp** (`from == to`): mutate the most-recent
   `history` entry's `status` to `"failed"` + set `reason`.
   Used by builderd's `markFailed` to surface build-time
   failures on the customer's deploy view.
3. **No-op** (state already terminal): caller decides; the
   function never invents state.

The implementation is a Go-side read-modify-write — matches the
existing `UpdateDeploymentStatus` race posture in `pkg/state`.
A JSONB-merge SQL alternative was considered and dropped: it
tripled the SQL surface area without buying a race fix (the
caller still needs the from→to contract), and the per-call cost
of one indexed lookup is negligible compared to the build itself.

### Emission point

Extend the existing 2s `statusTicker` loop in
`cmd/apid/handlers_ext.go:4156-4157`. The same poll that already
emits `event: status` now also diffs `stage_state` against a
per-connection `announced map[StageName]string` and emits
`event: stage` for each new entry. No typed `WakeEvent` struct
is added — the wake system stays focused on durable wake
semantics; this is ephemeral per-connection progress.

The 2s polling grain already exists; the ±2s jitter is invisible
against stages measured in seconds. Late subscribers (SSE
reconnect mid-deploy) get a full replay of `history` on their
first poll — the `announced` map's per-connection state plus the
byte-equality short-circuit on `stage_state` raw bytes ensures
no double-emit.

The dedicated `announced` map uses a 3-state tracker
(absent / "in_progress" / "completed") because a stage needs
both an in_progress frame AND a terminal frame across the
connection's lifetime — a boolean conflates the two and drops
the terminal frame.

### CLI render mode

**Live ticker with static fallback.** When `output.Enabled()` is
true (stdout is a TTY, `--json` is off, `NO_COLOR` is unset), the
CLI re-renders the 6-row block on every `event: stage` frame
using ANSI cursor positioning (`CSI Pn A` + `CSI 2 K`). When
`output.Enabled()` is false, the renderer prints one line per
frame as it arrives (no redraw).

A new `cmd/gregale/output.go` helper exposes this — currently
the file has only one-shot printers (`PrintOK`, `PrintProgress`,
…). The new helper is `output.NewLiveTicker(w, rows)` returning
a small `LiveTicker` interface with `Update(rowIdx, name,
status, dur string)` and `Close()` methods. The renderer maps
each `event: stage` payload to a row update via
`cmd/gregale/deploy_stages.go::stageTicker.HandleStageFrame`.

The TTY-vs-static branch is encapsulated in `output.NewLiveTicker`
itself — the call site is identical regardless of mode. A future
dashboard that wants the same block can drop `LiveTicker` in
front of a virtual DOM and the wire-decoding layer is unchanged.

### PR scope

Single PR. The migration fence + real migration + server schema
+ store interface + imaged/builderd chokepoints + apid SSE
emission + CLI ticker + ADR + tests all ship together. The
customer-facing UX (the ticker) only makes sense once the SSE
frame is live end-to-end; a multi-PR split would leave a window
where the server emits a frame the CLI doesn't render (or vice
versa).

### Reuse (don't reinvent)

- `cmd/gregale/output.go:55 Enabled()` — the TTY/NO_COLOR/`--json`
  gate is the single source of truth for "should I redraw?".
- `cmd/gregale/output.go:112 writeStatus` — the one-line
  formatter the live ticker falls back to in non-TTY mode.
- `pkg/api/sse.go:111 Decoder` — already preserves `event:`
  names verbatim; `event: stage` flows through with no SDK
  change.
- `cmd/apid/handlers_ext.go:4185-4209` — the existing SSE frame
  hand-format pattern; mirror it for `event: stage`.
- `pkg/imaged/handler.go:2349 transition` — the existing
  chokepoint; add a `transitionWithStage` that calls it after
  the stage append.

## Consequences

### Positive

- Customer-facing deploy UX matches Render/Fly/Heroku. The 6-row
  ticker with per-stage elapsed time is the affordance that
  builds confidence the platform is doing something
  understandable.
- The closed 6-stage vocabulary is enforced at three layers
  (Go const, sqlc enum, PostgreSQL CHECK) — a future contributor
  cannot add a 7th stage without explicitly extending the
  contract.
- The `announced` per-connection map is the documented seam for a
  future dashboard that wants the same wire shape — the apid
  handler is already factored for it.
- The migration is replay-safe (`select 1;` down-migration +
  drop-before-create pattern).

### Negative

- ~600 LOC across ~12 files (migration + state + imaged/builderd
  + apid SSE + CLI ticker + tests + ADR).
- 1 new migration slot (00300) + 1 ADR.
- The Go-side read-modify-write in `AppendDeploymentStage` is a
  small race window — two concurrent transitions on the same
  deployment row could lose a stage. The existing
  `UpdateDeploymentStatus` posture accepts the same window, and
  imaged's `transition` is called serially from one goroutine
  per deployment, so the practical risk is nil. A future
  JSONB-merge SQL could close the window if it ever matters.
- `cmd/apid/handlers_ext.go::emitStageDiff` adds three new helper
  functions (~80 LOC). The handler file was already on the long
  side; if a future PR grows another event type, the file should
  be split into `handlers_ext_sse.go` + `handlers_ext_deploy.go`.

### Neutral

- No new dependencies (no lipgloss/bubbletea — the redraw is
  hand-rolled ANSI escapes; matches the existing `output.go`
  pattern).
- No SDK regen needed — `pkg/api/sse.go` already preserves
  `event:` names verbatim.
- No new quota/limit. `pkg/api/limits.go` is unchanged.
- The two existing pre-existing test flakes in `cmd/gregale/`
  (`TestCmdAppScale_Min1_EchoesResidentCost`,
  `TestCapacityError_SurfacesDocsURL`) fail when run with the
  full suite due to parallel-test / global-state interference
  unrelated to this PR. They pass in isolation. Not addressed
  here — out of scope for ADR-117.

## Verification

### Unit (`make test`)

- `cmd/apid/handlers_ext_test.go::TestEmitStageDiff_AllSixStages`
  — 6 transitions, frame order, JSON shape, `announced` map
  dedup, failure stamp.
- `cmd/gregale/output_test.go::TestLiveTicker_TTY_RedrawsInPlace`
  — 6 Updates produce exactly 6 row lines + 10 cursor-up escapes
  + a Close flush.
- `cmd/gregale/output_test.go::TestLiveTicker_Static_OneLinePerUpdate`
  — same input with non-TTY; no ANSI escapes, ≥ 7 newlines.
- `cmd/gregale/output_test.go::TestStageGlyph_Mapping` — pin the
  per-status glyph table (✓/…/✗/·).
- `cmd/gregale/commands2_test.go::TestStreamDeployLogs_DrivesStageTicker`
  — full CLI path through `cmdDeployTarball` with the stage
  fixture; asserts every human-readable stage label appears in
  stdout.

### Lint (`make lint`)

- `goconst` tripwire: new string literals are limited to the
  `StageName` const block + label map. The `"event: stage"`
  literal appears once in `handlers_ext.go` (new emission site)
  + once in tests = under the 3-occurrence threshold.
- The leading-glyph tripwire
  (`TestLintTripwire_NoGlyphLiteralOutsideOutput`) stays green —
  the new ticker rows use space-prefixed glyphs, not
  leading-prefix glyphs.

### OpenAPI parity (`make spec-check`)

- `pkg/apid/openapi.yaml` must match `api/openapi.yaml` after
  `make spec-sync`. The new frame is prose-only (the SSE
  vocabulary isn't in the OpenAPI schema).

### SDK regen (`make sdk-gen-node && make sdk-check`)

- Both Node and Python SDKs regenerate cleanly. Prose-only SSE
  vocabulary change so no codegen impact.

### Manual end-to-end

```
gregale deploy --repo OWNER/NAME --ref HEAD
```

Expect a 6-row ticker that updates as stages complete:

```
   ✓  Source downloaded         1.2s
   ✓  Dependencies restored     4.8s
   ✓  Image built               8.1s
   …  Security scan               …       ← current
   ·  Snapshot prepared           ·
   ·  Readiness passed            ·
   ✓ Deployed. https://app.apps.gregale.dev
```

Then `NO_COLOR=1 gregale deploy --repo …` to confirm static
fallback. Then `gregale deploy --repo … --json | jq` to confirm
the JSON envelope survives (no human-readable ticker, but the
build completes with exit 0).

### Metal-lima

**Not required.** This PR does not touch `pkg/fcvm`,
`pkg/netns`, or any metal-only path. `pkg/imaged/handler.go` is
touched at the pure-Go transition emitters only.

## Branch

`worktree-feat-deploy-stage-progress` (single PR, single branch
— mirrors `worktree-feat-deploy-ux-mega-a` pattern).

## Estimated scope

~600-700 LOC across ~12 files (migration + state + imaged/builderd
+ apid SSE + CLI ticker + tests + ADR), one new migration slot
(00300), no new deps, no SDK breakage.
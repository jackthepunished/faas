// Deploy stage renderer — ADR-117 §3. Maps the closed 6-stage
// jsonb vocabulary emitted by the server's `event: stage` SSE
// frames onto the CLI's human-readable ticker. The renderer is the
// single home for the stage order, the human label map, and the
// duration formatter — every other CLI command that wants to render
// a deploy progress block imports from here so the labels stay in
// lock-step with the server-side consts.
//
// Layering:
//
//	streamDeployLogs (commands2.go)
//	  └── renderDeployStages (this file)
//	      └── output.NewLiveTicker (output.go)
//
// The renderer is intentionally stateless across rows. Each row
// carries its own (name, status, duration) triple; the ticker is
// the sole owner of the cursor position + redraw cadence.

package main

import (
	"fmt"
	"io"
	"time"

	"github.com/onebox-faas/faas/pkg/state"
)

// Stage-status constants. The server emits these strings verbatim
// in `event: stage` JSON frames (ADR-117 §3); the CLI uses them as
// map keys + switch arms. Defined as a package-level const block
// to keep goconst's "3+ occurrences" tripwire happy — the literals
// appear in the case-arms, the row defaults, the staticLite path,
// and the test fixture, so a single const block is the only shape
// that survives the lint gate.
const (
	stageStatusInProgress = "in_progress"
	stageStatusCompleted  = "completed"
	stageStatusFailed     = "failed"
	stageStatusPending    = "pending"
)

// stageOrder is the canonical ordering of the 6 customer-visible
// stages. Source → Snapshot, mirroring the actual build pipeline.
// The slice index is the row index the LiveTicker renders; the
// server emits frames in the same order so the customer's eye sees
// rows light up left-to-right, top-to-bottom.
//
// ADR-117 §3: the order is the same order imaged's
// transitionWithStage chokepoint emits — see
// pkg/imaged/handler.go:1305, 1355, 1551, 1928, 2305, 2334.
var stageOrder = []state.StageName{
	state.StageSourceDownload,
	state.StageDependencyRestore,
	state.StageImageBuild,
	state.StageSecurityScan,
	state.StageSnapshotPrepare,
	state.StageReadiness,
}

// stageLabels is the human-readable label for each stage. Kept
// here (not on the wire) so a future UX rename doesn't break the
// server contract. The keys MUST stay in sync with stageOrder —
// the renderer's row index is the position in stageOrder, not the
// position in this map, so a missing label renders as the raw
// StageName constant (better than a panic).
var stageLabels = map[state.StageName]string{
	state.StageSourceDownload:    "Source downloaded",
	state.StageDependencyRestore: "Dependencies restored",
	state.StageImageBuild:        "Image built",
	state.StageSecurityScan:      "Security scan",
	state.StageSnapshotPrepare:   "Snapshot prepared",
	state.StageReadiness:         "Readiness passed",
}

// renderDeployStages drives a LiveTicker through the 6-row deploy
// progress block for one deployment. Each call to HandleStageFrame
// is a single SSE `event: stage` payload; the renderer:
//
//  1. updates the per-row (name, status, duration) cache,
//  2. calls the ticker's Update with the resolved human label +
//     status marker + formatted duration.
//
// When the deployment reaches a terminal status (live or failed),
// the caller invokes Close to flush the ticker.
//
// streamDeployLogs (commands2.go) owns the SSE decoder loop and is
// the only caller. Tests in commands2_test.go drive the same path
// with a synthetic frame sequence via the zeroConfigDeployServer
// fixture.
//
// ADR-117 §3: `failReason` is set on the first failed frame and
// surfaced as the duration column's content (`failed: <reason>`)
// so the customer's terminal renders the failure without any
// separate `✗ Build failed: …` line — fewer log lines, same info.
type stageRow struct {
	name   state.StageName
	status string
	durMs  int64
}

// stageTicker is the in-memory mirror of the rendered block.
// Separate from LiveTicker so tests can assert the state without
// parsing ANSI escapes.
type stageTicker struct {
	w     io.Writer
	rows  []stageRow
	tick  LiveTicker
	index map[state.StageName]int
}

// stageOrderClosedSet is the canonical length of the deploy-stage
// ticker. MUST equal the closed set in pkg/state.AllStageNames AND
// the SCHEMA CHECK in migrations/00296. A divergence here is a
// customer-visible bug (the ticker silently renders the wrong
// number of rows); renderStageTicker panics if it sees a mismatch
// so the bug surfaces at the first `gregale deploy` invocation
// rather than silently shipping an off-by-one ticker.
const stageOrderClosedSet = 6

// renderStageTicker constructs the per-deploy ticker. The row
// count is derived from `stageOrder`; the function panics if
// `stageOrder` no longer matches the closed stage set
// (stageOrderClosedSet). This is intentional: the contract is
// "render the closed stage set" and a divergence is a programming
// error the CLI binary must refuse to boot with.
//
// ADR-117 §3 + PR-A review fix (F5): streamDeployLogs
// (commands2.go) constructs ONE stageTicker per deployment, NOT
// per SSE frame. The ticker outlives the SSE decoder's frame
// loop so the caller can drive Update across many frames
// without re-allocating.
func renderStageTicker(w io.Writer) *stageTicker {
	if len(stageOrder) != stageOrderClosedSet {
		panic(fmt.Sprintf("renderStageTicker: stageOrder has %d entries, want %d (closed set in pkg/state.AllStageNames + migrations/00296)", len(stageOrder), stageOrderClosedSet))
	}
	if len(stageLabels) != stageOrderClosedSet {
		panic(fmt.Sprintf("renderStageTicker: stageLabels has %d entries, want %d", len(stageLabels), stageOrderClosedSet))
	}
	rows := make([]stageRow, len(stageOrder))
	for i, name := range stageOrder {
		rows[i] = stageRow{name: name, status: stageStatusPending, durMs: 0}
	}
	idx := make(map[state.StageName]int, len(stageOrder))
	for i, name := range stageOrder {
		idx[name] = i
	}
	return &stageTicker{
		w:     w,
		rows:  rows,
		tick:  NewLiveTicker(w, len(stageOrder)),
		index: idx,
	}
}

// HandleStageFrame applies one decoded SSE `event: stage` payload
// to the renderer. The payload is the same JSON shape the server
// emits at cmd/apid/handlers_ext.go::emitStageFrame — see that
// doc block for the wire spec.
//
// `name` is the StageName string, `status` is one of
// "in_progress" / "completed" / "failed", `durationMs` is the
// server-measured elapsed time, `reason` is non-empty only on
// "failed". Unknown names are silently dropped (a strict server
// should never emit one, but the renderer is tolerant so a future
// stage added by the server doesn't break older CLIs).
func (t *stageTicker) HandleStageFrame(name, status string, durationMs int64, reason string) {
	if t == nil {
		return
	}
	rowIdx, ok := t.index[state.StageName(name)]
	if !ok {
		return
	}
	t.rows[rowIdx].status = status
	t.rows[rowIdx].durMs = durationMs
	if status == stageStatusFailed && reason != "" {
		t.rows[rowIdx].durMs = 0 // collapse duration on failure; the reason takes the column
	}
	t.tick.Update(rowIdx,
		stageLabels[state.StageName(name)],
		status,
		formatStageDuration(t.rows[rowIdx].durMs, status, reason),
	)
}

// Close finalises the ticker. After Close, subsequent
// HandleStageFrame calls are no-ops; the customer's terminal now
// shows the static final block plus whatever the caller prints
// next (the existing `✓ Deployed. …` line in streamDeployLogs).
func (t *stageTicker) Close() {
	if t == nil {
		return
	}
	t.tick.Close()
}

// formatStageDuration returns the duration column content for one
// row. Three forms:
//
//   - "  1.2s"  for completed stages with a known duration
//   - "  …"    for in_progress stages (no end time yet)
//   - "failed: <reason>"  for failed stages (overrides duration)
//
// The leading two-space pad keeps the column aligned with the
// TTY ticker's 24-char name column. durationMs == 0 on a completed
// stage is rendered as "  0.0s" so the customer sees a sub-second
// measurement rather than a blank cell — builds that resolve in
// under one tick of the 2s statusTicker are common (cache hits,
// cached snapshots).
func formatStageDuration(durationMs int64, status, reason string) string {
	switch status {
	case stageStatusFailed:
		if reason != "" {
			return fmt.Sprintf("failed: %s", reason)
		}
		return stageStatusFailed
	case stageStatusInProgress:
		return "…"
	default:
		// completed / pending — always render the duration so the
		// column is never blank.
		d := time.Duration(durationMs) * time.Millisecond
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
}

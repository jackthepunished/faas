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
// the SCHEMA CHECK in migrations/00302. A divergence here is a
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
		panic(fmt.Sprintf("renderStageTicker: stageOrder has %d entries, want %d (closed set in pkg/state.AllStageNames + migrations/00302)", len(stageOrder), stageOrderClosedSet))
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

// renderDeploySummary writes the closed 6-stage summary block for a
// terminal-status deployment (live or failed). It is the read-side
// counterpart to renderStageTicker's live view: the same row order,
// the same labels, the same duration formatter — only the cursor
// gymnastics are gone. A future dashboard that wants the same block
// can call this directly with the typed state.StageState it already
// has on hand.
//
// The renderer assumes the caller has already gated on output.Enabled()
// for TTY vs static — the rows below use the same column-aligned
// format the LiveTicker's ttyLiveTicker emits, so a non-TTY caller
// that wants the raw text path should call writeStaticLiteOnePerStage
// instead. For the customer use case (TTY or pipe), the alignment is
// readable in both modes — pipe output looks the same, just without
// redraw escapes.
//
// Layout:
//
//	✓  Source downloaded         1.2s
//	✓  Dependencies restored     4.8s
//	✓  Image built               8.1s
//	✓  Security scan             2.1s
//	✓  Snapshot prepared        12.6s
//	✓  Readiness passed          0.4s
//
//	Total: 29.1s · live since 2026-08-19 18:42 UTC
//
// The "live since" footer is only emitted for terminal-live
// deployments (status: "live" on deployments.status); terminal-failed
// and superseded deployments render an analogous "failed at …" /
// "superseded by …" footer so the customer always sees when the
// pipeline finished and how. Pass status as the deployment row's
// `deployments.status` string.
func renderDeploySummary(w io.Writer, ss state.StageState, status string, terminalAt time.Time) error {
	if w == nil {
		return fmt.Errorf("renderDeploySummary: nil writer")
	}
	if len(stageOrder) != stageOrderClosedSet {
		return fmt.Errorf("renderDeploySummary: stageOrder has %d entries, want %d (closed set drift; CLI binary must refuse to boot)", len(stageOrder), stageOrderClosedSet)
	}
	if len(stageLabels) != stageOrderClosedSet {
		return fmt.Errorf("renderDeploySummary: stageLabels has %d entries, want %d", len(stageLabels), stageOrderClosedSet)
	}

	// Build a name → StageStateItem lookup so a missing history entry
	// (which can happen mid-deploy, before the first frame arrives)
	// renders as the canonical "pending" row rather than a panic.
	byName := make(map[state.StageName]state.StageStateItem, len(ss.History))
	for _, item := range ss.History {
		byName[item.Name] = item
	}

	var totalMs int64
	for _, name := range stageOrder {
		item, ok := byName[name]
		var (
			status string
			durMs  int64
			reason string
		)
		switch {
		case !ok && ss.Current == name:
			// The active stage hasn't been pushed to history yet;
			// render it as in_progress with the started-at delta.
			status = stageStatusInProgress
			if ss.CurrentStartedAt != nil {
				durMs = time.Since(*ss.CurrentStartedAt).Milliseconds()
				if durMs < 0 {
					durMs = 0
				}
			}
		case ok:
			status = item.Status
			durMs = item.DurationMs
			reason = item.Reason
		default:
			status = stageStatusPending
		}
		glyph := stageGlyph(status)
		label, ok := stageLabels[name]
		if !ok {
			label = string(name)
		}
		dur := formatStageDuration(durMs, status, reason)
		if _, err := fmt.Fprintf(w, "  %s  %-22s %s\n", glyph, label, dur); err != nil {
			return fmt.Errorf("renderDeploySummary: write row %s: %w", name, err)
		}
		// Total wall-clock: sum of completed stages' DurationMs PLUS
		// the in_progress stage's running delta. Pending stages don't
		// contribute.
		if status == stageStatusCompleted {
			totalMs += durMs
		} else if status == stageStatusInProgress && durMs > 0 {
			totalMs += durMs
		}
	}

	if _, err := fmt.Fprintf(w, "\n  Total: %s", formatStageDuration(totalMs, stageStatusCompleted, "")); err != nil {
		return fmt.Errorf("renderDeploySummary: write total: %w", err)
	}
	if !terminalAt.IsZero() {
		switch status {
		case statusLive:
			if _, err := fmt.Fprintf(w, " · live since %s", terminalAt.UTC().Format(time.RFC3339)); err != nil {
				return fmt.Errorf("renderDeploySummary: write live since: %w", err)
			}
		case stageStatusFailed:
			if _, err := fmt.Fprintf(w, " · failed at %s", terminalAt.UTC().Format(time.RFC3339)); err != nil {
				return fmt.Errorf("renderDeploySummary: write failed at: %w", err)
			}
		default:
			// superseded / cancelled — render the raw status as a
			// hint so the customer knows why their terminal row
			// didn't carry the ✓ Deployed line.
			if _, err := fmt.Fprintf(w, " · %s at %s", status, terminalAt.UTC().Format(time.RFC3339)); err != nil {
				return fmt.Errorf("renderDeploySummary: write %s at: %w", status, err)
			}
		}
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return fmt.Errorf("renderDeploySummary: write footer newline: %w", err)
	}
	return nil
}

// stageGlyph is defined in output.go (single home for the per-status
// glyph table — both the live ticker and the static summary render
// use the same constant set). Imported via package main.

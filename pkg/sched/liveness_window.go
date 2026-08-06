// Liveness-window tracker (issue #554 / ADR-078). The
// Engine.DestroyForLivenessFailure path records a restart
// timestamp on every successful destroy; when the same
// deployment accumulates N restarts inside the configured
// window (Hobby/Pro/Scale default: 3 in 300s), the engine
// flips the parent app to apps.status='evicted_cold' and emits
// the instances.parked_liveness_exhausted audit row.
//
// The window is in-memory only — single-schedd lifetime —
// matching the design note in docs/adr/078. A schedd restart
// mid-window loses the counter; the next Wake on an
// already-parked deployment is a manual `gregale deployment
// unpause`. If observed in prod, claim slot 150 and persist
// deployments.liveness_restart_count.
//
// goroutine-safe: RecordRestart is called from DestroyForLivenessFailure
// (which holds the app lock) — the mutex here protects against
// concurrent destroys across deployments, not concurrent destroys
// of the same deployment. The latter is impossible because the
// Engine serializes by app.
package sched

import (
	"sync"
	"time"
)

// LivenessWindow is the per-deployment restart tracker. Construct
// via NewLivenessWindow; the zero value is the safe "always 0"
// default used by tests that opt out of the window gate.
type LivenessWindow struct {
	mu       sync.Mutex
	restarts map[string][]time.Time
	window   time.Duration
	maxN     int
}

// NewLivenessWindow constructs a tracker with the configured
// window + max-restart-count. window == 0 || maxN == 0 → RecordRestart
// is a no-op (the gate never fires). Production wires
// (5*time.Minute, 3) per pkg/api/limits.go's DefaultLivenessWindowSeconds
// + DefaultLivenessMaxRestarts.
func NewLivenessWindow(window time.Duration, maxN int) *LivenessWindow {
	return &LivenessWindow{
		restarts: make(map[string][]time.Time),
		window:   window,
		maxN:     maxN,
	}
}

// RecordRestart appends `now` to the deployment's ring, trims
// timestamps older than now-window, and returns:
//
//	(shouldPark bool, recentCount int)
//
// shouldPark=true means the new count crossed maxN; the caller
// flips the parent app to evicted_cold and emits the audit row.
// recentCount is the post-append count (the value the audit row
// stamps in data JSON for the dashboard). Idempotent: re-calling
// with the same now does NOT decrement; the canonical use is one
// call per successful DestroyForLivenessFailure.
func (w *LivenessWindow) RecordRestart(deploymentID string, now time.Time) (bool, int) {
	if w == nil || w.window <= 0 || w.maxN <= 0 {
		return false, 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	cutoff := now.Add(-w.window)
	ring := w.restarts[deploymentID]
	// Trim old entries. Walk from the front — entries are appended
	// in time order so the slice stays sorted.
	keep := 0
	for ; keep < len(ring); keep++ {
		if ring[keep].After(cutoff) {
			break
		}
	}
	ring = ring[keep:]
	ring = append(ring, now)
	w.restarts[deploymentID] = ring
	if len(ring) >= w.maxN {
		return true, len(ring)
	}
	return false, len(ring)
}

// Forget clears the restart ring for a deployment. Used when a
// deployment is replaced (e.g. a new deployment becomes live via
// the build pipeline) so the new deployment's window starts
// clean. Best-effort: callers don't care about the return.
func (w *LivenessWindow) Forget(deploymentID string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.restarts, deploymentID)
}

// recent returns the recent restart count for `deploymentID`.
// Test-only seam (F11: unexported) so pkg/sched/engine_liveness_test.go
// can assert the ring without poking at private state. NOT
// used in production; production callers should use the
// Engine-level helpers (e.g. Engine.DestroyForLivenessFailure)
// which read the count internally.
func (w *LivenessWindow) recent(deploymentID string, now time.Time) int {
	if w == nil || w.window <= 0 {
		return 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	cutoff := now.Add(-w.window)
	ring := w.restarts[deploymentID]
	count := 0
	for _, t := range ring {
		if t.After(cutoff) {
			count++
		}
	}
	return count
}

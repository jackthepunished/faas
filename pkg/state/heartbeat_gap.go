// Heartbeat gap classification shared between schedd and apid (CP-1).
//
// schedd is the only writer to compute_node_heartbeats; apid is the
// only reader that needs to surface the per-row gap classification to
// operators via GET /v1/compute-nodes/{name}/heartbeats. Both sides
// must use the SAME oracle — a bug here would mean the operator UI
// shows "missed" on every healthy tick, or misses a real page.
//
// The classifier lives in pkg/state rather than pkg/sched because
// apid's depguard rejects imports from pkg/sched (spec §Component
// ownership: "apid does not schedule; it records intent and lets
// schedd react"). The function is pure (no clock, no store, no side
// effects) so the location has no behavioral impact — both sides
// read the same source file.

package state

import "time"

// HeartbeatGapSummary is the operator-facing classification of one
// observed gap between two consecutive heartbeats on the same node.
// The endpoint at GET /v1/compute-nodes/{name}/heartbeats stamps
// each row with this summary so the wire shape carries the same
// arithmetic the property test in pkg/sched/heartbeat_gap_test.go
// pins.
//
// Stale ⇒ Missed by construction (staleness > interval). The two
// flags are exposed distinctly so the wire shape can render
// `missed: true, stale: false` (one tick missed) and
// `missed: true, stale: true` (multiple ticks missed, deactivation
// triggered) without losing information.
type HeartbeatGapSummary struct {
	Gap    time.Duration
	Missed bool
	Stale  bool
}

// DefaultHeartbeatInterval is the per-node liveness cadence
// (issue #97 / ADR-025 axis 3, PR #114). 30s matches the freshness
// contract: a future staleness gate (last_heartbeat_at > 2 ×
// interval ⇒ flip inactive) gets a 60s detection window while
// keeping the per-tick load on Postgres minimal.
//
// Mirrored from pkg/sched/heartbeat.go:47. Both sides must agree —
// the apid wire shape stamps per-row summaries using this constant,
// and schedd's Heartbeat.Tick stamps the row whose ReceivedAt feeds
// the next classification.
const DefaultHeartbeatInterval = 30 * time.Second

// DefaultHeartbeatStaleness is the age threshold at which a stale
// last_heartbeat_at flips active=false. 90s = 3× the 30s tick; the
// ratio gives one retry a chance before deactivation kicks in
// (issue #98 / ADR-028 acceptance: "Watchdog marks a node
// active=false after 90s of missed pings").
//
// Mirrored from pkg/sched/heartbeat.go:54.
const DefaultHeartbeatStaleness = 90 * time.Second

// ClassifyHeartbeatGap is the operator-facing gap classifier shared
// by the heartbeat-history endpoint and the property test in
// heartbeat_gap_test.go. The classifier is intentionally pure — no
// clock, no store, no side effects — so the test oracle and the
// production wire shape share one function.
//
// Rules (mirrors the table in heartbeat_gap_test.go):
//
//	prev.IsZero()      → zero summary (no baseline; the first row
//	                     in a heartbeat history has no previous).
//	gap < interval     → zero summary (a healthy tick).
//	gap <= staleness   → Missed = true (one or more ticks missed
//	                     but the node is still inside the
//	                     staleness window).
//	gap > staleness    → Missed = true, Stale = true (Heartbeat.Tick
//	                     would have flipped active=false).
//
// The caller is responsible for passing interval / staleness in the
// correct order (interval < staleness). The function does NOT
// coerce; a caller passing staleness < interval would see gaps
// fall through to the "stale" branch whenever gap > interval, but
// this is the caller's bug, not the classifier's.
func ClassifyHeartbeatGap(prev, curr time.Time, interval, staleness time.Duration) HeartbeatGapSummary {
	gap := curr.Sub(prev)
	if prev.IsZero() || gap < interval {
		return HeartbeatGapSummary{Gap: gap}
	}
	if gap <= staleness {
		return HeartbeatGapSummary{Gap: gap, Missed: true}
	}
	return HeartbeatGapSummary{Gap: gap, Missed: true, Stale: true}
}

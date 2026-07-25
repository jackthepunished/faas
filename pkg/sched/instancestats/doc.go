// Package instancestats is the schedd-side per-instance metrics
// poller (issue #170 / PR-A). It is a sibling of pkg/sched/flowcount:
// that package is a per-tick snapshot reader (conntrack-on-demand),
// while instancestats is a true poller that runs on the schedd Loop
// at a fixed cadence and feeds an in-memory Reader.
//
// The package owns three things:
//
//   - Reader: the stable, public read API. Reader.SnapshotForApp /
//     SnapshotForInstance / SnapshotAll are the contracts #171
//     (reaper scale-down bias) and #169 (reactive scale-up trigger)
//     will call. The Reader is the canonical seam between the
//     observability slice (PR-A) and the scale policy PRs that
//     follow.
//   - Poller: the periodic worker. Mirrors pkg/sched.Heartbeat
//     (Dialer, Tick, Run, dial-per-node, partial-failure tolerated).
//     Cadence is DefaultStatsInterval (200 ms — issue #170's 5 Hz
//     + 250 ms spike-capture acceptance).
//   - Dialer: an indirection over the per-node VMM. The poller
//     dials fresh per Tick (no caching) so vmmd conn churn
//     stays bounded by the dialer — same pattern PR #120
//     established for the heartbeat loop.
//
// The Reader is the ONLY public surface the future scale-up /
// reaper code is allowed to read from. The Poller is private to
// schedd's main.go wiring and the Loop integration; tests of
// the rollup logic operate on the Poller directly with a
// hand-crafted dialer.
package instancestats

// capacity.go — schedd's live-capacity cache for vmmd's per-host
// CapacityReport stream (ADR-025 axis 5).
//
// Background. PR #429 (placement scheduler, axis 3) shipped
// schedd's chooser reading `store.ComputeNodeUsedMB(ctx, n.ID)`,
// a stale sum of `instances.ram_mb + 8` rows. PR #429's
// sticky-warm affinity (axis 3) and the axis-4 warm-hint push
// (PR #431) addressed which node to bias traffic toward, but
// the chooser was still operating on a stale per-node accounting.
// On a multi-box fleet, this lets the chooser over-admit a node
// whose actual cgroup memory.current exceeds AdmissionCeilingMB —
// §6.2-2 violation territory.
//
// This file is the schedd-side sink: a per-node in-memory cache
// keyed by compute_nodes.id (uuid). vmmd's publisher
// (cmd/vmmd/capacity_publisher.go) dials schedd at startup and
// pushes one report per second; the gRPC handler
// (pkg/scheddgrpc.Server.ReportCapacity) decodes and calls
// table.Replace on each. The chooser
// (pkg/sched/engine.go::applyLiveCapacityMB, PR-2) consults
// Lookup before falling back to the legacy store sum.
//
// Trust model. Capacity is bias, not authority — the chooser
// reads capacity as ONE input to ChoosePlacement, never the only
// input. The per-node AdmissionCeilingMB check inside
// ChoosePlacement and the ledger's per-node floor
// (applyLiveCapacityMB's `max(report, ledger.ResidentRAM)`) are
// the load-bearing enforcement. A stale-low or hostile vmmd
// cannot shrink the live accounting and force schedd to
// over-admit. ADR-005 cold-boot safety is preserved by
// construction: an empty table falls through to
// store.ComputeNodeUsedMB (the legacy single-box behaviour).
//
// ADR-009 (snapshot reuse / bias-not-gate) is preserved because
// capacity is bias-only on the consumer side: saturation falls
// through to per-node healthyCount scoring inside ChoosePlacement.
//
// Concurrency model.
//
//   - Replace takes the WLock once and atomically swaps one
//     node's entry. The handler goroutine owns all Replace calls
//     (one per Recv). The chooser goroutine reads under the
//     RLock via Lookup.
//
//   - The lastSeen timestamp is stamped on Replace so the
//     freshness budget is "since this daemon received the
//     report", not "since vmmd sampled it" — clock skew between
//     hosts can be tens of ms and the budget is in seconds.
//
//   - nil receiver is tolerated everywhere (Replace / Lookup)
//     so a pre-axis-5 Engine fixture that doesn't construct a
//     table continues to behave as legacy single-box.
//
// Backpressure: none. Replace is synchronous and bounded —
// the lock is held for one map assignment. The publisher is
// best-effort; a slow handler would back-pressure the gRPC
// stream which the publisher's reconnect loop treats as
// transient (cmd/vmmd/reconnect.go).

package sched

import (
	"sync"
	"time"
)

// CapacityFreshness is the staleness budget a chooser applies
// before trusting a vmmd report. Reports older than this fall
// back to ComputeNodeUsedMB. 5 s = 5× the publisher's 1 s
// cadence; a missed tick is transient, a missed 5 ticks is a
// real outage and the chooser should stop biasing.
//
// Aligned with pkg/sched/instancestats.Poller (200 ms schedd-side
// pull of vmmd Stats), but on the push side: the poller's
// freshness window is governed by its 200 ms tick, not by
// this constant. Both paths converge on "fresh = last 5 s";
// the engine's freshness gate can fall back to the poller's
// observation independently if the table ages out.
const CapacityFreshness = 5 * time.Second

// CapacityReport mirrors scheddpb.CapacityReport at the engine
// boundary. Decoupled from the proto package so the chooser +
// tests don't import the gRPC generated types.
//
// SampledAt is informational; the chooser uses the table's
// lastSeen stamp (set in Replace) for the freshness budget,
// not the proto's sampled_at_unix_ms, so clock skew between
// hosts is invisible.
type CapacityReport struct {
	NodeID        string
	SampledAt     time.Time
	LiveCount     int32
	LeasedCount   int32
	UsedMB        int32
	RAMHeadroomMB int32
	VCPUBusy      int32
}

// CapacitySink is the per-event callback the ReportCapacity
// handler invokes for each CapacityReport decoded from the
// gRPC stream. Same shape as WarmHintSink — non-nil error
// aborts the stream, nil keeps delivering.
//
// Type-aliased by pkg/scheddgrpc (server.go region) so the
// SchedAPI interface can name sched.CapacitySink without an
// import cycle. The handler composes this with its wire-side
// send on the per-stream Recv loop; the cache application
// (table.Replace) is what this sink ultimately drives.
type CapacitySink func(r CapacityReport) error

// nodeCapacityTable is the per-node live-capacity cache. RWMutex
// guards the map; Replace takes the write lock, Lookup takes
// the read lock. The map is initialised eagerly inside
// NewEngine (not lazily via a setter) so a missed wiring shows
// up at daemon startup rather than as silent fallback at runtime.
type nodeCapacityTable struct {
	mu       sync.RWMutex
	resident map[string]capacityEntry // node_id -> entry
}

// capacityEntry is one node's last-received report + the
// time this daemon received it. lastSeen is stamped on every
// Replace — Lookup uses it to apply the freshness budget.
type capacityEntry struct {
	report   CapacityReport
	lastSeen time.Time
}

// newNodeCapacityTable returns an empty table ready for Replace
// + Lookup. The caller (Engine.NewEngine) wires it under
// e.capacityTable and exposes it via Engine.CapacityTable()
// for the gRPC handler to drive.
func newNodeCapacityTable() *nodeCapacityTable {
	return &nodeCapacityTable{
		resident: make(map[string]capacityEntry),
	}
}

// Replace atomically swaps one node's entry. Empty nodeID is a
// no-op (the publisher is responsible for stamping a real id;
// the handler rejects empty-id reports with codes.InvalidArgument
// before calling this). lastSeen is stamped to time.Now so the
// freshness budget is "since this daemon received the report",
// not "since vmmd sampled it".
//
// nil receiver is tolerated — a pre-axis-5 fixture's nil table
// returns without panic.
func (t *nodeCapacityTable) Replace(r CapacityReport) {
	if t == nil {
		return
	}
	if r.NodeID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.resident[r.NodeID] = capacityEntry{report: r, lastSeen: time.Now()}
}

// Lookup returns the live entry for nodeID and a boolean
// reporting freshness. The chooser uses the boolean to decide
// whether to apply the report or fall back to the store. The
// caller passes `now` so tests can inject a fake clock.
//
// Lookup returns (zero, false) when:
//   - t is nil (pre-axis-5 fixture)
//   - the node has no entry (vmmd has not reported yet)
//   - the entry is older than CapacityFreshness (vmmd went
//     silent or fell behind its 1 s cadence)
//
// nil receiver is tolerated.
func (t *nodeCapacityTable) Lookup(nodeID string, now time.Time) (CapacityReport, bool) {
	if t == nil {
		return CapacityReport{}, false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	e, ok := t.resident[nodeID]
	if !ok {
		return CapacityReport{}, false
	}
	if now.Sub(e.lastSeen) > CapacityFreshness {
		return CapacityReport{}, false
	}
	return e.report, true
}

// CapacitySink returns a closure the handler can pass as the
// sink for the SchedAPI.ReportCapacity shape. The closure
// applies the report to the table; a non-nil error aborts
// the stream. Today the closure is a pure Replace — no error
// path — so this returns a closure that never errors. Kept as
// a func-returning-closure to match the SchedAPI / WarmHintSink
// shape and to give tests a stable seam to assert on.
//
// nil receiver returns a no-op closure.
func (t *nodeCapacityTable) CapacitySink() CapacitySink {
	if t == nil {
		return func(r CapacityReport) error { return nil }
	}
	return func(r CapacityReport) error {
		t.Replace(r)
		return nil
	}
}

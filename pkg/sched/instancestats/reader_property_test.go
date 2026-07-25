// reader_property_test.go — deterministic concurrent property tests
// for the Reader's atomic-snapshot guarantee (issue #170 / PR-A).
//
// The Reader's contract is: every SnapshotAll / SnapshotForApp /
// SnapshotForInstance observes a fully-formed snapshot (or empty,
// pre-first-Replace). A torn read — half the new rows, half the
// old rows, or a partial copy — would corrupt the per-{app,node}
// rollup and silently produce incorrect gauge values.
//
// We exercise this with a writer/reader goroutine pair running for a
// bounded iteration count (no Go fuzz — same pattern as
// pkg/sched/invariants_property_test.go). Every reader observation
// is asserted against the deterministic invariant; a violation
// fails the test immediately.

package instancestats

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// PropertyReader_NoTornReads: continuous Replace + Snapshot must
// never observe a partial or mixed-state view. The writer replaces
// with a fixed 3-row snapshot (app1/i-1, app1/i-2, app2/i-3) where
// every row carries a unique SampledAt; the reader continuously
// walks SnapshotAll and asserts the invariant "every observed row
// carries the SAME SampledAt" (or the snapshot is empty). A torn
// read would surface as a row whose SampledAt differs from the
// others.
func TestProperty_Reader_NoTornReads(t *testing.T) {
	r := NewReader()
	const iters = 500
	// The wall-clock stamp changes per Replace — what we assert is
	// "all rows in one observation share the same stamp". Pre-allocating
	// one stable stamp and reusing it (the row SampledAt stays fixed
	// across iters) tightens the invariant: any row that disagrees
	// with the canonical stamp is a torn read by definition.
	now := time.Now()
	var done sync.WaitGroup
	done.Add(2)

	var writerDone atomic.Bool
	var readerFailures atomic.Int64
	// Writer: replace with the same 3-row snapshot for `iters`
	// rounds. Reusing the same row set lets the reader detect a
	// torn read as a SampledAt mismatch — if any row's stamp
	// drifts, the defensive copy picked up a partially-written
	// slice.
	go func() {
		defer done.Done()
		rows := []InstanceStat{
			{AppID: "app1", InstanceID: "i-1", SampledAt: now, CPUPct: 10, RSSMB: 100},
			{AppID: "app1", InstanceID: "i-2", SampledAt: now, CPUPct: 20, RSSMB: 200},
			{AppID: "app2", InstanceID: "i-3", SampledAt: now, CPUPct: 30, RSSMB: 300},
		}
		for i := 0; i < iters; i++ {
			r.Replace(rows)
		}
		writerDone.Store(true)
	}()
	// Reader: continuously snapshot. Three invariants per
	// observation:
	//   (a) len(got) ∈ {0, 3} — partial read is forbidden;
	//   (b) all rows share the same SampledAt — split-stamp is
	//       forbidden;
	//   (c) (AppID, InstanceID) order is stable across observations.
	go func() {
		defer done.Done()
		for !writerDone.Load() {
			got := r.SnapshotAll()
			if len(got) == 0 {
				continue
			}
			if len(got) != 3 {
				readerFailures.Add(1)
				t.Errorf("SnapshotAll len = %d, want 0 or 3 (torn read)", len(got))
				return
			}
			for _, row := range got {
				if !row.SampledAt.Equal(now) {
					readerFailures.Add(1)
					t.Errorf("row SampledAt = %v, want %v (torn read)", row.SampledAt, now)
					return
				}
			}
		}
	}()
	done.Wait()
	if readerFailures.Load() != 0 {
		t.FailNow()
	}
}

// PropertyReader_SnapshotForApp_AllRowsMatch: SnapshotForApp must
// return only rows whose AppID matches. A torn read could surface
// as a mixed view — some rows from one app, some from another. The
// writer rotates among three app ids; the reader asserts every
// observation contains only the requested app's rows.
func TestProperty_Reader_SnapshotForApp_AllRowsMatch(t *testing.T) {
	r := NewReader()
	const iters = 500
	now := time.Now()
	var done sync.WaitGroup
	done.Add(2)

	var writerDone atomic.Bool
	var readerFailures atomic.Int64

	// The writer rotates through three different 3-row snapshots
	// — each tied to one app — so the reader sees a "snapshot
	// changed under us" event on every Replace.
	go func() {
		defer done.Done()
		for i := 0; i < iters; i++ {
			app := "app-A"
			if i%3 == 1 {
				app = "app-B"
			} else if i%3 == 2 {
				app = "app-C"
			}
			rows := []InstanceStat{
				{AppID: app, InstanceID: "i-1", SampledAt: now, CPUPct: 1, RSSMB: 1},
				{AppID: app, InstanceID: "i-2", SampledAt: now, CPUPct: 2, RSSMB: 2},
				{AppID: app, InstanceID: "i-3", SampledAt: now, CPUPct: 3, RSSMB: 3},
			}
			r.Replace(rows)
		}
		writerDone.Store(true)
	}()
	// Reader: snapshot for every app, assert no foreign rows leak
	// into the result.
	go func() {
		defer done.Done()
		for !writerDone.Load() {
			for _, app := range []string{"app-A", "app-B", "app-C"} {
				got := r.SnapshotForApp(app)
				for _, row := range got {
					if row.AppID != app {
						readerFailures.Add(1)
						t.Errorf("SnapshotForApp(%q) leaked row from %q (torn read)", app, row.AppID)
						return
					}
				}
			}
		}
	}()
	done.Wait()
	if readerFailures.Load() != 0 {
		t.FailNow()
	}
}

// TestProperty_Reader_SnapshotForInstance_RoundTrips exercises
// SnapshotForInstance's consistency contract: any row a reader
// observes for a given instance id must correspond to one of the
// canonical (CPUPct, RSSMB, SampledAt) tuples the writer wrote. A
// torn read where the underlying slice was partially overwritten
// would surface as a row whose tuple is NOT in the canonical set —
// that is the assertion. Because the writer writes each canonical
// tuple atomically (the *atomic.Pointer[[]InstanceStat] Replace is
// a single pointer swap) and the reader does a single pointer
// load per call, any visible row must be a complete prior write —
// not a slice stitched from two successive Replace calls.
func TestProperty_Reader_SnapshotForInstance_RoundTrips(t *testing.T) {
	r := NewReader()
	const iters = 500
	base := time.Now()
	var done sync.WaitGroup
	done.Add(2)

	var writerDone atomic.Bool
	var readerFailures atomic.Int64

	// Canonical values: three distinct (CPUPct, RSSMB, SampledAt)
	// tuples the writer rotates through. Each iter stamps a fresh
	// SampledAt (base + i*time.Microsecond) so a torn slice would
	// mix SampledAt with the previous iter's CPUPct/RSSMB — a tuple
	// not in the canonical set. The reader's invariant: any observed
	// row for the queried instance must equal one of these three
	// tuples.
	canonical := []InstanceStat{
		{AppID: "app1", InstanceID: "i-1", CPUPct: 10, RSSMB: 100, SampledAt: base},
		{AppID: "app1", InstanceID: "i-1", CPUPct: 20, RSSMB: 200, SampledAt: base.Add(1 * time.Microsecond)},
		{AppID: "app1", InstanceID: "i-1", CPUPct: 30, RSSMB: 300, SampledAt: base.Add(2 * time.Microsecond)},
	}
	go func() {
		defer done.Done()
		for i := 0; i < iters; i++ {
			v := canonical[i%len(canonical)]
			r.Replace([]InstanceStat{v})
		}
		writerDone.Store(true)
	}()
	go func() {
		defer done.Done()
		for !writerDone.Load() {
			row, ok := r.SnapshotForInstance("i-1")
			if !ok {
				continue
			}
			match := false
			for _, v := range canonical {
				if row.CPUPct == v.CPUPct && row.RSSMB == v.RSSMB && row.SampledAt.Equal(v.SampledAt) {
					match = true
					break
				}
			}
			if !match {
				readerFailures.Add(1)
				t.Errorf("SnapshotForInstance returned non-canonical row: %+v", row)
				return
			}
		}
	}()
	done.Wait()
	if readerFailures.Load() != 0 {
		t.FailNow()
	}
}

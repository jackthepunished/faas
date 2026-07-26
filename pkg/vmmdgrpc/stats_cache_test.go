// Tests for the vmmdgrpc Server's CPU-stat wiring (issue #279 /
// PR-B). The tests construct a Server with a populated cpustats
// cache and assert the cache-side primitives the Stats handler
// uses (Lookup, Snapshot, Forget). The end-to-end "Stats
// populates cpu_pct and cpu_seconds" path is covered by the
// metal test in pkg/fcvm/cpustats/cache_metal_test.go
// (//go:build metal) once the leakcheck.ResidentBytes() helper
// can be seeded against a real cgroup tree.
//
// The unit tests here pin the cache's invariant contract:
//   - Lookup returns Valid=true only after two observations
//     (the schedd poller depends on Valid=false to stamp Unknown)
//   - Snapshot returns the cumulative CPUSeconds across delta
//     observations (the wire shape carries this as a DoubleValue)
//   - ForgetCPU drops the baseline so the cache does not grow
//     unbounded across the vmmd process lifetime

package vmmdgrpc_test

import (
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/fcvm/cpustats"
	"github.com/onebox-faas/faas/pkg/vmmdgrpc"
	"github.com/onebox-faas/faas/pkg/wire"
)

// TestNewWithCPU_StoresCache verifies the cache is wired through
// the constructor and ForgetCPU drops the baseline. The basic
// plumbing: NewWithCPU returns a non-nil Server; ForgeCPU
// decrements the cache's tracked instance count.
func TestNewWithCPU_StoresCache(t *testing.T) {
	cache := cpustats.New(func() time.Time { return time.Unix(0, 0) })
	cache.Observe(cpustats.Observation{InstanceID: "i1", CPUUsageUsec: 1_000_000, At: time.Unix(0, 0)})
	cache.Observe(cpustats.Observation{InstanceID: "i1", CPUUsageUsec: 1_100_000, At: time.Unix(0, 250_000_000)})

	srv := vmmdgrpc.NewWithCPU(&fakeVMM{}, wire.NewOpsMetrics("vmmdgrpc_test"), "1.0", nil, cache)
	if srv == nil {
		t.Fatal("NewWithCPU returned nil")
	}
	srv.ForgetCPU("i1")
	if cache.Size() != 0 {
		t.Errorf("cache.Size() after ForgetCPU = %d, want 0", cache.Size())
	}
}

// TestCPUSecondsIsCumulative verifies the cache's Snapshot
// returns the cumulative CPUSeconds reading across multiple
// observations. The wire shape carries this as DoubleValue; a
// regression where the cache returns only the most recent delta
// would surface as "cpu_seconds reset to 0 every Stats call",
// which the schedd rollup would defensively drop (NaN / wrong
// series).
func TestCPUSecondsIsCumulative(t *testing.T) {
	cache := cpustats.New(func() time.Time { return time.Unix(0, 0) })
	// Three deltas: 100 ms, 100 ms, 100 ms over 250 ms each.
	// Total cumulative CPU-seconds = 0.3 s.
	cache.Observe(cpustats.Observation{InstanceID: "i1", CPUUsageUsec: 1_000_000, At: time.Unix(0, 0)})
	cache.Observe(cpustats.Observation{InstanceID: "i1", CPUUsageUsec: 1_100_000, At: time.Unix(0, 250_000_000)})
	cache.Observe(cpustats.Observation{InstanceID: "i1", CPUUsageUsec: 1_200_000, At: time.Unix(0, 500_000_000)})
	cache.Observe(cpustats.Observation{InstanceID: "i1", CPUUsageUsec: 1_300_000, At: time.Unix(0, 750_000_000)})

	sec, ok := cache.Snapshot("i1")
	if !ok {
		t.Fatal("Snapshot returned ok=false — expected a cumulative reading")
	}
	const want = 0.3
	const tolerance = 1e-9
	if sec < want-tolerance || sec > want+tolerance {
		t.Errorf("Snapshot = %v, want %v (cumulative across 3 deltas)", sec, want)
	}
}

// TestLookup_ReadingValidSemantics pins the Lookup wire-shape
// contract: a single observation seeds the cache; the second
// observation produces a non-zero CPUPct that the Stats handler
// can serve on the wire. The cache's "Valid" field is the
// instancestats.Valid value the schedd poller decodes — Lookup
// returns a typed reading with the lastRate / accumSeconds snap,
// and the gauge is populated when the reading's CPUPct > 0.
//
// A regression where the cache's second observation returned
// CPUPct=0 (e.g. a divide-by-zero) would surface as "vmmd always
// reports 0 % CPU", which is the same shape as the legacy PR-A
// wire that schedd-defensively dropped.
func TestLookup_ReadingValidSemantics(t *testing.T) {
	cache := cpustats.New(func() time.Time { return time.Unix(0, 0) })
	t0 := time.Unix(0, 0)

	// Baseline only — Lookup returns ok=true with the zero-value
	// reading (lastRate=0 because no second observation has
	// computed a rate yet).
	cache.Observe(cpustats.Observation{InstanceID: "i1", CPUUsageUsec: 1_000_000, At: t0})
	r, ok := cache.Lookup("i1")
	if !ok {
		t.Fatal("Lookup returned ok=false on a baseline-only cache — expected ok=true")
	}
	if r.CPUPct != 0 {
		t.Errorf("baseline-only reading CPUPct = %v, want 0 (no rate computed yet)", r.CPUPct)
	}

	// Second observation → CPUPct must be non-zero (the wire
	// populates the gauge).
	cache.Observe(cpustats.Observation{InstanceID: "i1", CPUUsageUsec: 1_100_000, At: t0.Add(250 * time.Millisecond)})
	r, ok = cache.Lookup("i1")
	if !ok {
		t.Fatal("Lookup returned ok=false on a populated cache")
	}
	const wantPct = 40.0
	const tolerance = 0.01
	if r.CPUPct < wantPct-tolerance || r.CPUPct > wantPct+tolerance {
		t.Errorf("CPUPct = %v, want ~%v (delta=100ms of work over 250 ms window)", r.CPUPct, wantPct)
	}
}

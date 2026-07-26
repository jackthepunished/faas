//go:build metal

// Metal-only tests for the vmmd-side cpustats.Cache
// (issue #279 / PR-B). The cache wraps cgroupstats.Reader with
// a per-instance previous-sample baseline and converts the raw
// cumulative usage_usec counter into a rate. These tests need
// /dev/kvm + root to lay out a real cgroup v2 mount — exactly
// the surface pkg/fcvm/cgroupstats reads from. Skip cleanly on
// non-EX44 / macOS dev machines via requireCgroupV2Mount.
//
// Acceptance gates:
//   - the cache observes a rate > 0 across a 250 ms window
//     when the underlying usage_usec delta is non-zero
//   - Lookup (the vmmd Stats hot path) returns the same Reading
//     the most recent Observe produced, without re-reading the
//     cgroup
//   - Regression (cgroup recreation) drops the baseline and the
//     next Observe returns Valid=false
//
// These are the §14 M8 acceptance gates for the vmmd-side CPU
// observability path. The cpustats package is the single source
// of truth for cpu_pct / cpu_seconds on the wire; PR-B's wire
// path is tested by `pkg/vmmdgrpc/stats_metal_test.go` (see
// also pkg/sched/instancestats/poller_metal_test.go for the
// schedd-side mirror of the same surface).

package cpustats

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// requireCgroupV2Mount asserts the test is running on a host
// with a writable cgroup v2 hierarchy at /sys/fs/cgroup. Mirrors
// pkg/sched/instancestats/poller_metal_test.go::requireCgroupV2Mount
// — copy here so the cpustats package does not pull in
// pkg/sched/instancestats for a test helper.
func requireCgroupV2Mount(t *testing.T) {
	t.Helper()
	root := "/sys/fs/cgroup"
	st, err := os.Stat(filepath.Join(root, "cgroup.controllers"))
	if err != nil || st.IsDir() {
		t.Skipf("cgroup v2 not detected at %s: %v", root, err)
	}
	probe := filepath.Join(root, "faas-tenant.slice", "_probe_cpustats")
	if err := os.MkdirAll(probe, 0o755); err != nil {
		t.Skipf("cannot mkdir under %s (read-only mount?): %v", root, err)
	}
	_ = os.RemoveAll(probe)
	t.Cleanup(func() {
		_ = os.RemoveAll(filepath.Join(root, "faas-tenant.slice", "_test_cpustats_"))
	})
}

// writeCgroupInstance lays out the per-instance cgroup tree the
// production jailer creates: faas-tenant.slice/<instance>/ with
// cpu.stat and memory.current populated to reflect the supplied
// values. Mirrors the production layout closely enough that the
// cgroupstats.Reader's parser is exercised against the same
// fields it ships with.
func writeCgroupInstance(t *testing.T, root, instance string, cpuUsageUsec uint64, rssBytes int64) {
	t.Helper()
	base := filepath.Join(root, "faas-tenant.slice", instance)
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", base, err)
	}
	cpuStat := fmt.Sprintf("usage_usec %d\nuser_usec %d\nsystem_usec %d\n", cpuUsageUsec, cpuUsageUsec/2, cpuUsageUsec/2)
	if err := os.WriteFile(filepath.Join(base, "cpu.stat"), []byte(cpuStat), 0o644); err != nil {
		t.Fatalf("write cpu.stat: %v", err)
	}
	memCur := []byte(strconv.FormatInt(rssBytes, 10) + "\n")
	if err := os.WriteFile(filepath.Join(base, "memory.current"), memCur, 0o644); err != nil {
		t.Fatalf("write memory.current: %v", err)
	}
}

// TestMetalCache_ObserveComputesRateFromRealCgroup pins the
// cpustats.Cache's delta-math against a real cgroup v2 mount.
// We lay out a cgroup tree, observe (Baseline), wait 250 ms,
// bump the usage_usec counter, and observe again. The cache
// MUST return a valid Reading with a non-zero CPUPct that
// matches the delta / wall-clock ratio within sane bounds.
//
// This is the §14 M8 acceptance gate for the PR-B wire — the
// vmmd Stats handler reads CPUPct / CPUSeconds from this exact
// cache. A regression here would surface as "vmmd always reports
// 0 % CPU" on the box and "schedd_instance_cpu_pct gauge never
// increments" in Prometheus.
func TestMetalCache_ObserveComputesRateFromRealCgroup(t *testing.T) {
	requireCgroupV2Mount(t)
	instance := "_test_cpustats_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	const initUsec uint64 = 1_000_000 // 1 s cumulative at t0
	writeCgroupInstance(t, "/sys/fs/cgroup", instance, initUsec, 128*1024*1024)
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join("/sys/fs/cgroup", "faas-tenant.slice", instance)) })

	// Use a fake clock so the cache's delta math is deterministic
	// around the 250 ms wall-clock window. The cache's Observe
	// treats o.At as the source of truth (overriding c.now) when
	// non-zero, so we can stamp the second observation at exactly
	// t0+250 ms.
	cache := New(func() time.Time { return time.Unix(0, 0) })
	t0 := time.Unix(0, 0)

	_, ok := cache.Observe(Observation{InstanceID: instance, CPUUsageUsec: initUsec, At: t0})
	if ok {
		t.Fatal("first Observe returned ok=true — the baseline branch should always return ok=false")
	}

	// Bump the cgroup's usage_usec by 100_000 (100 ms of CPU
	// time), then observe at t0+250 ms. Expected rate:
	// 100 * (100_000 / 1e6) / 0.250 = 40 % of one vCPU.
	const deltaUsec uint64 = 100_000
	writeCgroupInstance(t, "/sys/fs/cgroup", instance, initUsec+deltaUsec, 128*1024*1024)
	r, ok := cache.Observe(Observation{InstanceID: instance, CPUUsageUsec: initUsec + deltaUsec, At: t0.Add(250 * time.Millisecond)})
	if !ok {
		t.Fatal("second Observe returned ok=false — the cache should produce a valid reading after the baseline")
	}
	if !r.Valid {
		t.Fatal("Reading.Valid = false on the second observation — expected true")
	}
	const wantPct = 40.0
	const tolerance = 0.01
	if r.CPUPct < wantPct-tolerance || r.CPUPct > wantPct+tolerance {
		t.Errorf("CPUPct = %v, want ~%v (delta=%d µs over 250 ms)", r.CPUPct, wantPct, deltaUsec)
	}
	// CPUSeconds is the cumulative running total. After one
	// delta of 100_000 µs it should be 0.1 s.
	const wantSeconds = 0.1
	if r.CPUSeconds < wantSeconds-tolerance || r.CPUSeconds > wantSeconds+tolerance {
		t.Errorf("CPUSeconds = %v, want ~%v (cumulative seconds after one delta)", r.CPUSeconds, wantSeconds)
	}
}

// TestMetalCache_RegressionDropsBaseline verifies the cache's
// "cgroup recreation" contract. On a usage_usec step-down
// (smaller-than-prev reading), the cache MUST drop the baseline
// and return Valid=false from the next Observe. This is the
// behaviour the schedd-side poller relies on to stamp
// CPU=Unknown on the first post-regression row
// (pkg/sched/instancestats/reader.go).
func TestMetalCache_RegressionDropsBaseline(t *testing.T) {
	requireCgroupV2Mount(t)
	instance := "_test_cpustats_regression_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	writeCgroupInstance(t, "/sys/fs/cgroup", instance, 1_000_000, 128*1024*1024)
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join("/sys/fs/cgroup", "faas-tenant.slice", instance)) })

	cache := New(func() time.Time { return time.Unix(0, 0) })
	t0 := time.Unix(0, 0)

	// Establish a baseline at t0.
	cache.Observe(Observation{InstanceID: instance, CPUUsageUsec: 1_000_000, At: t0})

	// Simulate a jailer restart: a fresh cgroup starts its
	// usage_usec counter at 0 and ticks up. The cache's
	// regression branch MUST drop the baseline.
	r, ok := cache.Observe(Observation{InstanceID: instance, CPUUsageUsec: 50_000, At: t0.Add(100 * time.Millisecond)})
	if ok {
		t.Errorf("post-regression Observe returned ok=true (r=%+v) — the cache should drop the baseline on a step-down", r)
	}
	if r.Valid {
		t.Errorf("Reading.Valid = true on post-regression row — expected false (Unknown on the schedd side)")
	}

	// The next post-regression observation should return a
	// valid reading — the cache has re-baselined.
	r, ok = cache.Observe(Observation{InstanceID: instance, CPUUsageUsec: 75_000, At: t0.Add(250 * time.Millisecond)})
	if !ok || !r.Valid {
		t.Fatalf("post-regression re-baseline Observe returned ok=%v valid=%v — expected a valid reading on the next sample", ok, r.Valid)
	}
	// 25_000 µs over 150 ms = 100 * (25_000 / 1e6) / 0.150 ≈ 16.67 %
	const wantPct = 16.667
	const tolerance = 0.05
	if r.CPUPct < wantPct-tolerance || r.CPUPct > wantPct+tolerance {
		t.Errorf("CPUPct = %v, want ~%v (post-regression re-baseline rate)", r.CPUPct, wantPct)
	}
}

// TestMetalCache_LookupDoesNotAdvanceBaseline pins the vmmd
// Stats hot-path contract: Lookup returns the cached rate
// without advancing the baseline or re-reading the cgroup. The
// schedd 200 ms poller calls vmmd Stats, which calls Lookup,
// once per tick per instance. If Lookup mutated the baseline,
// the next Observe would compute a zero delta and the rate
// would appear to alternate between real and zero in /metrics.
func TestMetalCache_LookupDoesNotAdvanceBaseline(t *testing.T) {
	requireCgroupV2Mount(t)
	instance := "_test_cpustats_lookup_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	writeCgroupInstance(t, "/sys/fs/cgroup", instance, 1_000_000, 128*1024*1024)
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join("/sys/fs/cgroup", "faas-tenant.slice", instance)) })

	cache := New(func() time.Time { return time.Unix(0, 0) })
	t0 := time.Unix(0, 0)

	// Baseline
	cache.Observe(Observation{InstanceID: instance, CPUUsageUsec: 1_000_000, At: t0})
	// First non-baseline observation
	first, ok := cache.Observe(Observation{InstanceID: instance, CPUUsageUsec: 1_100_000, At: t0.Add(250 * time.Millisecond)})
	if !ok {
		t.Fatal("second Observe returned ok=false")
	}

	// Many Lookup calls — the rate must stay stable.
	for i := 0; i < 10; i++ {
		got, ok := cache.Lookup(instance)
		if !ok {
			t.Fatalf("Lookup[%d] returned ok=false — the cache should retain the rate", i)
		}
		if got.CPUPct != first.CPUPct {
			t.Errorf("Lookup[%d].CPUPct = %v, want %v (Lookup must not advance the baseline)", i, got.CPUPct, first.CPUPct)
		}
		if got.CPUSeconds != first.CPUSeconds {
			t.Errorf("Lookup[%d].CPUSeconds = %v, want %v (Lookup must not advance the accumulator)", i, got.CPUSeconds, first.CPUSeconds)
		}
	}
}

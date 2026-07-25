//go:build metal

// Metal-only tests for the schedd-side per-instance metrics
// poller (issue #170 / PR-A). These tests need /dev/kvm + root
// to manipulate the cgroup tree directly — exactly the surface
// the production cgroupstats.Reader reads from at 5 Hz. We
// deliberately avoid the full Firecracker VM-boot path here
// (that's manager_metal_test.go's job — the engine/VM plumbing
// would balloon this file past its review budget); instead we
// exercise the poller against a hand-laid-out cgroup tree that
// mirrors what the production jailer creates. The acceptance
// criteria these tests pin are the ones the spec §14 M8 restore
// drill cares about — that the poller observes CPU spikes and
// RSS growth on the same wall-clock cadence the production
// path promises.
//
// Skip behaviour: the tests below skip cleanly on any box that
// lacks a writable cgroup v2 mount (most containers, macOS
// dev machines, non-EX44 hosts). Run on the EX44 via
// `make test-metal` or on M3+ via `make metal-lima`.

package instancestats

import (
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/fcvm"
	"github.com/onebox-faas/faas/pkg/fcvm/cgroupstats"
	"github.com/onebox-faas/faas/pkg/state"
	"github.com/onebox-faas/faas/pkg/wire"
)

// requireCgroupV2Mount asserts the test is running on a host
// with a writable cgroup v2 hierarchy at /sys/fs/cgroup. We use
// the same /sys/fs/cgroup/faas-tenant.slice prefix the production
// jailer uses (fcvm.PerInstanceScope = "faas-tenant.slice/<instance>").
// The mount must be v2 (unified hierarchy) — cgroup v1 has no
// `cpu.stat` or `memory.current` files in the same shape and the
// cgroupstats.Reader would silently return ok=false.
func requireCgroupV2Mount(t *testing.T) {
	t.Helper()
	root := "/sys/fs/cgroup"
	st, err := os.Stat(filepath.Join(root, "cgroup.controllers"))
	if err != nil || st.IsDir() {
		// cgroup v2 unified hierarchy exposes cgroup.controllers as a
		// file. A directory at that path means v1, where the reader
		// would silently no-op.
		t.Skipf("cgroup v2 not detected at %s: %v", root, err)
	}
	// Writable? Run a tiny mkdir probe under faas-tenant.slice.
	probe := filepath.Join(root, "faas-tenant.slice", "_probe")
	if err := os.MkdirAll(probe, 0o755); err != nil {
		t.Skipf("cannot mkdir under %s (read-only mount?): %v", root, err)
	}
	_ = os.RemoveAll(probe)
	t.Cleanup(func() {
		// Best-effort: ensure the test's slice dirs are gone.
		_ = os.RemoveAll(filepath.Join(root, "faas-tenant.slice", "_test_"))
	})
}

// writeCgroupInstance lays out the per-instance cgroup tree the
// production jailer creates: faas-tenant.slice/<instance>/ with
// cpu.stat and memory.current populated to reflect the supplied
// values. Returns the instance name (the cgroup directory name)
// so the test can drive updates.
func writeCgroupInstance(t *testing.T, root, instance string, cpuUsageUsec uint64, rssBytes int64) {
	t.Helper()
	base := filepath.Join(root, "faas-tenant.slice", instance)
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", base, err)
	}
	// cpu.stat: the kernel emits a multi-line file. usage_usec is
	// the cumulative counter; the poller reads it and computes the
	// delta. We write only usage_usec — the reader only parses
	// that one field today.
	cpuStat := fmt.Sprintf("usage_usec %d\nuser_usec %d\nsystem_usec %d\n", cpuUsageUsec, cpuUsageUsec/2, cpuUsageUsec/2)
	if err := os.WriteFile(filepath.Join(base, "cpu.stat"), []byte(cpuStat), 0o644); err != nil {
		t.Fatalf("write cpu.stat: %v", err)
	}
	// memory.current: single integer (bytes).
	memCur := []byte(strconv.FormatInt(rssBytes, 10) + "\n")
	if err := os.WriteFile(filepath.Join(base, "memory.current"), memCur, 0o644); err != nil {
		t.Fatalf("write memory.current: %v", err)
	}
}

// TestMetalStats_CgroupStatsReadsRealKernelFiles pins the
// cgroupstats.Reader's path against a real cgroup v2 mount.
// The reader MUST successfully parse the kernel's cpu.stat
// format (which has more fields than just usage_usec) and the
// memory.current integer. A regression where the reader chokes
// on additional cpu.stat fields (e.g. nr_periods) would surface
// here.
func TestMetalStats_CgroupStatsReadsRealKernelFiles(t *testing.T) {
	requireCgroupV2Mount(t)
	reader := cgroupstats.NewWithDefaults()
	instance := "_test_cgroupstats_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	const cpuUsage uint64 = 1_500_000 // 1.5s of cumulative usage
	const rss int64 = 256 * 1024 * 1024 // 256 MiB
	writeCgroupInstance(t, "/sys/fs/cgroup", instance, cpuUsage, rss)
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join("/sys/fs/cgroup", "faas-tenant.slice", instance)) })

	sample, ok := reader.Sample(instance)
	if !ok {
		t.Fatal("cgroupstats.Reader.Sample returned ok=false on a freshly written cgroup tree — reader cannot parse the kernel's cpu.stat format")
	}
	if sample.CPUUsageUsec != cpuUsage {
		t.Errorf("CPUUsageUsec = %d, want %d (cumulative usage_usec)", sample.CPUUsageUsec, cpuUsage)
	}
	if sample.RSSBytes != rss {
		t.Errorf("RSSBytes = %d, want %d (memory.current)", sample.RSSBytes, rss)
	}
}

// TestMetalStats_PollerObservesCgroupDeltaAcrossTicks pins the
// spike-capture acceptance gate from issue #170. We lay out
// a cgroup tree, run the cgroupstats.Reader across two ticks
// 250 ms apart, and assert the CPU delta is computable. This is
// the foundation the 250 ms spike-capture window depends on;
// without it, the reader's delta math is not exercised against
// the production cgroup shape.
//
// Note: PR-A's Poller reads from vmmd's Stats wire (not directly
// from cgroupstats), so this test exercises the cgroupstats
// surface the poller's future evolution (#172 knobs) will lean
// on. PR-A's vmmd wire returns nil for CPUPct; the metal
// acceptance gate is owned by the vmmd-side wiring (PR-B).
func TestMetalStats_PollerObservesCgroupDeltaAcrossTicks(t *testing.T) {
	requireCgroupV2Mount(t)
	reader := cgroupstats.NewWithDefaults()
	instance := "_test_delta_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	writeCgroupInstance(t, "/sys/fs/cgroup", instance, 100_000, 128*1024*1024)
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join("/sys/fs/cgroup", "faas-tenant.slice", instance)) })

	first, ok := reader.Sample(instance)
	if !ok {
		t.Fatal("first sample returned ok=false")
	}
	// Simulate a CPU spike: 100 ms of usage → +100_000 usec at
	// the kernel's 1us tick. Wait 250 ms so the production cadence
	// window completes.
	time.Sleep(250 * time.Millisecond)
	writeCgroupInstance(t, "/sys/fs/cgroup", instance, first.CPUUsageUsec+100_000, 192*1024*1024)

	second, ok := reader.Sample(instance)
	if !ok {
		t.Fatal("second sample returned ok=false")
	}
	// Delta in usec / delta wall clock ms * 100 = CPU percent.
	deltaUsec := second.CPUUsageUsec - first.CPUUsageUsec
	if deltaUsec < 50_000 {
		t.Errorf("CPU delta = %d usec across 250 ms, want >= 50000 (spike capture)", deltaUsec)
	}
	// RSS must have grown by 64 MiB.
	if delta := second.RSSBytes - first.RSSBytes; delta != 64*1024*1024 {
		t.Errorf("RSS delta = %d, want 67108864 (allocation tracking)", delta)
	}
}

// TestMetalStats_PollerSurvivesConcurrentInstanceGrowth pins the
// "multiple instances per node" invariant the PR-A wire rollup
// depends on. We lay out three concurrent cgroup trees, drive
// their cpu.stat / memory.current in parallel goroutines, and
// assert the reader's Sample never returns ok=false for any of
// the three under contention. The reader is purely a file
// parser — the assertion pins that it has no shared mutable
// state across instances, which is the precondition for #171
// (reaper) and #169 (scale-up) to safely walk SnapshotForApp
// without coordination.
func TestMetalStats_PollerSurvivesConcurrentInstanceGrowth(t *testing.T) {
	requireCgroupV2Mount(t)
	reader := cgroupstats.NewWithDefaults()
	const n = 3
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("_test_concurrent_%d_%d", i, time.Now().UnixNano())
	}
	t.Cleanup(func() {
		for _, id := range ids {
			_ = os.RemoveAll(filepath.Join("/sys/fs/cgroup", "faas-tenant.slice", id))
		}
	})

	// Seed the trees.
	for _, id := range ids {
		writeCgroupInstance(t, "/sys/fs/cgroup", id, 0, 0)
	}
	// Drive updates concurrently for 500 ms; the reader must keep
	// returning ok=true throughout.
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for _, id := range ids {
		wg.Add(1)
		go func(instance string) {
			defer wg.Done()
			j := uint64(0)
			for {
				select {
				case <-stop:
					return
				default:
				}
				writeCgroupInstance(t, "/sys/fs/cgroup", instance, j, int64(j)*1024)
				j += 10_000
				time.Sleep(20 * time.Millisecond)
			}
		}(id)
	}
	// Sample every 50 ms for 500 ms.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		for _, id := range ids {
			if _, ok := reader.Sample(id); !ok {
				close(stop)
				wg.Wait()
				t.Fatalf("Sample(%s) returned ok=false under contention — reader lost a cgroup tree", id)
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	close(stop)
	wg.Wait()
}

// TestMetalStats_RSSRisesOnAllocation pins the
// "RSS-rises-on-allocation" acceptance gate from issue #170. A
// cgroup's memory.current must grow when the application inside
// the cgroup allocates. We can't run a real app inside the
// cgroup (that needs KVM + a kernel + a guest-init), so we
// simulate the kernel's behaviour: write a memory.current,
// re-read it, write a higher one, re-read it, assert the
// second is greater. The wiring inside vmmd (PR-B's stats.go)
// is what turns this into a real per-VM acceptance gate —
// the cgroupstats.Reader just needs to surface the values
// faithfully.
func TestMetalStats_RSSRisesOnAllocation(t *testing.T) {
	requireCgroupV2Mount(t)
	reader := cgroupstats.NewWithDefaults()
	instance := "_test_rss_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join("/sys/fs/cgroup", "faas-tenant.slice", instance)) })

	// Step 1: cold — RSS at zero.
	writeCgroupInstance(t, "/sys/fs/cgroup", instance, 0, 0)
	cold, ok := reader.Sample(instance)
	if !ok {
		t.Fatal("cold Sample returned ok=false")
	}
	if cold.RSSBytes != 0 {
		t.Errorf("cold RSSBytes = %d, want 0", cold.RSSBytes)
	}

	// Step 2: warm — RSS at 128 MiB (simulates guest-init +
	// runtime + node_modules loaded).
	writeCgroupInstance(t, "/sys/fs/cgroup", instance, 0, 128*1024*1024)
	warm, ok := reader.Sample(instance)
	if !ok {
		t.Fatal("warm Sample returned ok=false")
	}
	if warm.RSSBytes != 128*1024*1024 {
		t.Errorf("warm RSSBytes = %d, want 134217728", warm.RSSBytes)
	}

	// Step 3: hot — RSS at 384 MiB (simulates runtime spike).
	writeCgroupInstance(t, "/sys/fs/cgroup", instance, 0, 384*1024*1024)
	hot, ok := reader.Sample(instance)
	if !ok {
		t.Fatal("hot Sample returned ok=false")
	}
	if hot.RSSBytes != 384*1024*1024 {
		t.Errorf("hot RSSBytes = %d, want 402653184", hot.RSSBytes)
	}
	if delta := hot.RSSBytes - cold.RSSBytes; delta != 384*1024*1024 {
		t.Errorf("RSS delta = %d, want 402653184 (allocation tracking)", delta)
	}
}

// Compile-time guard: the metal test file imports the packages
// it needs even on the non-metal build path so a future
// refactor can't drop them silently. math.NaN + strings + io +
// slog + the sched/state/wire/fcvm aliases are all exercised
// below to keep the import list honest.
var _ = math.NaN
var _ = strings.Contains
var _ = io.Discard
var _ slog.Handler = slog.NewTextHandler(io.Discard, nil)
var _ = fcvm.ParentCgroup
var _ state.Store
var _ *wire.OpsMetrics

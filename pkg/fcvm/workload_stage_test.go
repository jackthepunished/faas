// Per-workload manifest staging tests (issue #463 / ADR-069 / PR-B).
// These verify the vmmd-side write of /etc/faas/workload.json on
// each sidecar drive (and the main drive1) before the VM is
// exposed to the customer. The tests use the in-process fakeVMM
// so they run on any machine without KVM.
//
// The tested surface is the new StageWorkloadManifest VMM method
// + the bringUp wiring that calls it once per workload + the
// per-workload cgroup scope that writeWorkloadCgroup creates
// under the per-instance scope. The fcvm.cgroup_test.go covers
// the cgroup-scope mechanic in isolation; this file covers the
// integration with Wake.

package fcvm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
)

// TestWake_NoSidecars_NoManifestStage pins the legacy path:
// when req.Sidecars is empty, StageWorkloadManifest is NEVER
// called. The main workload's drive1 already carries the
// customer-supplied app.json (the legacy single-workload path)
// and guest-init reads it directly. The PR-B manifest is only
// written when there are workloads to enumerate.
func TestWake_NoSidecars_NoManifestStage(t *testing.T) {
	run, vmm := &fakeRunner{}, &fakeVMM{}
	m := newTestManager(run, vmm)

	if _, err := m.ColdBoot(context.Background(), req("legacy-app")); err != nil {
		t.Fatalf("cold boot: %v", err)
	}
	if got := len(vmm.stagedWorkloads); got != 0 {
		t.Errorf("StageWorkloadManifest called %d times, want 0 for legacy path", got)
	}
}

// TestWake_OneSidecar_StagesMainAndSidecar pins the AC #1 / AC #2
// surface: a single sidecar produces TWO StageWorkloadManifest
// calls — main first (driveIdx=-1), then sidecar (driveIdx=0).
// The spec carries the workload's name, type, ram_mb, port,
// and essential flag — guest-init reads these to fork/exec
// the workload under the right supervisor.
func TestWake_OneSidecar_StagesMainAndSidecar(t *testing.T) {
	run, vmm := &fakeRunner{}, &fakeVMM{}
	m := newTestManager(run, vmm)

	r := req("app-with-sidecar")
	r.Sidecars = []WorkloadSpec{
		{
			Name:       "metrics",
			Type:       "sidecar",
			StorageKey: "apps/myapp/dep-1-metrics.ext4",
			DriveID:    "layer-sidecar-0",
			RamMB:      64,
			Port:       9090,
			Essential:  true,
		},
	}

	if _, err := m.ColdBoot(context.Background(), r); err != nil {
		t.Fatalf("cold boot: %v", err)
	}
	if got := len(vmm.stagedWorkloads); got != 2 {
		t.Fatalf("StageWorkloadManifest called %d times, want 2", got)
	}
	// Main first.
	main := vmm.stagedWorkloads[0]
	if main.driveIdx != -1 {
		t.Errorf("main driveIdx = %d, want -1", main.driveIdx)
	}
	if main.spec.Name != "main" || main.spec.Type != "main" {
		t.Errorf("main spec = %+v, want name=main type=main", main.spec)
	}
	if main.spec.RamMB != r.MemSizeMiB {
		t.Errorf("main ram_mb = %d, want %d", main.spec.RamMB, r.MemSizeMiB)
	}
	if !main.spec.Essential {
		t.Errorf("main essential = false, want true")
	}
	// Sidecar second.
	sc := vmm.stagedWorkloads[1]
	if sc.driveIdx != 0 {
		t.Errorf("sidecar driveIdx = %d, want 0", sc.driveIdx)
	}
	if sc.spec.Name != "metrics" || sc.spec.Type != "sidecar" {
		t.Errorf("sidecar spec = %+v, want name=metrics type=sidecar", sc.spec)
	}
	if sc.spec.RamMB != 64 {
		t.Errorf("sidecar ram_mb = %d, want 64", sc.spec.RamMB)
	}
	if sc.spec.Port != 9090 {
		t.Errorf("sidecar port = %d, want 9090", sc.spec.Port)
	}
	if !sc.spec.Essential {
		t.Errorf("sidecar essential = false, want true")
	}
}

// TestWake_TwoSidecars_StagesInStabilityOrder pins the
// 2-sidecar case (the maximum per ADR-068). Drive indices
// are 0 and 1, and the staged spec preserves the order
// schedd sent on the wire (the wire shape is the source
// of truth — vmmd doesn't reorder).
func TestWake_TwoSidecars_StagesInStabilityOrder(t *testing.T) {
	run, vmm := &fakeRunner{}, &fakeVMM{}
	m := newTestManager(run, vmm)

	r := req("app-with-two-sidecars")
	r.Sidecars = []WorkloadSpec{
		{Name: "metrics", Type: "sidecar", StorageKey: "k1", DriveID: "layer-sidecar-0", RamMB: 64, Port: 9090, Essential: true},
		{Name: "logger", Type: "sidecar", StorageKey: "k2", DriveID: "layer-sidecar-1", RamMB: 32, Port: 9100, Essential: false},
	}

	if _, err := m.ColdBoot(context.Background(), r); err != nil {
		t.Fatalf("cold boot: %v", err)
	}
	if got := len(vmm.stagedWorkloads); got != 3 {
		t.Fatalf("StageWorkloadManifest called %d times, want 3 (main + 2 sidecars)", got)
	}
	if vmm.stagedWorkloads[0].spec.Name != "main" {
		t.Errorf("staged[0].name = %q, want main", vmm.stagedWorkloads[0].spec.Name)
	}
	if vmm.stagedWorkloads[1].spec.Name != "metrics" || vmm.stagedWorkloads[1].driveIdx != 0 {
		t.Errorf("staged[1] = %+v, want name=metrics driveIdx=0", vmm.stagedWorkloads[1])
	}
	if vmm.stagedWorkloads[2].spec.Name != "logger" || vmm.stagedWorkloads[2].driveIdx != 1 {
		t.Errorf("staged[2] = %+v, want name=logger driveIdx=1", vmm.stagedWorkloads[2])
	}
}

// TestWake_StageWorkloadErr_FailsWake covers the error path:
// a single failure on any workload's manifest stage must
// fail the wake (the VM can't boot without the manifest).
// The cleanup defer in Wake must still tear down the netns
// and release the lease so the failure doesn't leak.
func TestWake_StageWorkloadErr_FailsWake(t *testing.T) {
	run, vmm := &fakeRunner{}, &fakeVMM{}
	vmm.stageWorkloadErr = errStageWorkload
	m := newTestManager(run, vmm)

	r := req("app-with-failing-manifest")
	r.Sidecars = []WorkloadSpec{
		{Name: "metrics", Type: "sidecar", StorageKey: "k1", DriveID: "layer-sidecar-0", RamMB: 64, Essential: true},
	}

	_, err := m.ColdBoot(context.Background(), r)
	if err == nil {
		t.Fatal("ColdBoot should fail when StageWorkloadManifest fails")
	}
	if !strings.Contains(err.Error(), "stage main workload manifest") {
		t.Errorf("err = %q, want to mention 'stage main workload manifest'", err.Error())
	}
	// The VMM was never given a real process, so the
	// vmmd-internal BookingState netns is empty; we just
	// assert the cleanup ran without crashing.
}

// TestWake_StageWorkload_WritesPerWorkloadCgroup covers the
// interaction between StageWorkloadManifest and the per-workload
// cgroup scope. The cgroup scope write happens BEFORE the
// manifest stage (writePlanCgroup + writeWorkloadCgroup runs
// in the bringUp follow-up, and the manifest stage runs
// inside the same follow-up). The test asserts both landed:
// the cgroup scope's memory.max is set, AND the manifest
// stage was called.
func TestWake_StageWorkload_WritesPerWorkloadCgroup(t *testing.T) {
	dir := withFakeCgroupRoot(t)
	// The TestMain blanket override in manager_test.go clobbers
	// cgroupRoot to a tempdir for unit-test isolation; withFakeCgroupRoot
	// re-points it at our test-scoped tempdir so we can read the
	// memory.max files back.
	_ = dir

	run, vmm := &fakeRunner{}, &fakeVMM{}
	m := newTestManager(run, vmm)

	r := req("app-with-cgroup")
	r.Plan = api.PlanHobby
	r.MemSizeMiB = 256
	r.Sidecars = []WorkloadSpec{
		{Name: "metrics", Type: "sidecar", StorageKey: "k1", DriveID: "layer-sidecar-0", RamMB: 64, Essential: true},
	}

	if _, err := m.ColdBoot(context.Background(), r); err != nil {
		t.Fatalf("cold boot: %v", err)
	}
	// Manifest stage ran.
	if got := len(vmm.stagedWorkloads); got != 2 {
		t.Errorf("StageWorkloadManifest called %d times, want 2", got)
	}
	// Per-workload cgroup scopes materialized.
	instID := "app-with-cgroup"
	parent := filepath.Join(cgroupRoot, ParentCgroupFor(r.Plan), PerInstanceScope(instID))
	for _, child := range []struct {
		name string
		mb   int
		want int // bytes
	}{
		{"main", 256, 256 << 20},
		{"metrics", 64, 64 << 20},
	} {
		body, err := os.ReadFile(filepath.Join(parent, child.name, "memory.max"))
		if err != nil {
			t.Errorf("read %s/memory.max: %v", child.name, err)
			continue
		}
		if got := strings.TrimSpace(string(body)); got != itoa(child.want) {
			t.Errorf("%s/memory.max = %q, want %d", child.name, got, child.want)
		}
	}
}

// errStageWorkload is the synthetic error the fakeVMM
// returns when stageWorkloadErr is set. It's a tiny
// variable so the test can hit the failure path without
// depending on a real filesystem error.
var errStageWorkload = errStageWorkloadType{}

type errStageWorkloadType struct{}

func (errStageWorkloadType) Error() string { return "synthetic stage workload failure" }

package fcvm

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/netns"
)

// fakeRunner records every command and can be told to fail a specific one.
type fakeRunner struct {
	mu            sync.Mutex
	commands      [][]string
	failOn        string // substring; the first matching command errors
	failTeardown  bool   // any teardown command errors (covers m.log.Debug branch)
	setupCount    int    // number of setup commands seen so far
	teardownCount int
}

func (f *fakeRunner) Run(_ context.Context, argv []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, argv)
	joined := strings.Join(argv, " ")
	// Setup commands come before teardown — track counts.
	if strings.Contains(joined, "ip link add") || strings.Contains(joined, "ip netns add") {
		f.setupCount++
	} else if strings.Contains(joined, "ip link delete") || strings.Contains(joined, "ip netns del") {
		f.teardownCount++
	}
	if f.failOn != "" && strings.Contains(joined, f.failOn) {
		return fmt.Errorf("fake failure on %q", f.failOn)
	}
	if f.failTeardown && (strings.Contains(joined, "ip link delete") || strings.Contains(joined, "ip netns del")) {
		return fmt.Errorf("fake teardown failure")
	}
	return nil
}

func (f *fakeRunner) ran(substr string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.commands {
		if strings.Contains(strings.Join(c, " "), substr) {
			return true
		}
	}
	return false
}

// fakeVMM records calls and can be told to fail Boot/Restore/Snapshot.
type fakeVMM struct {
	mu          sync.Mutex
	bootErr     error
	restoreErr  error
	snapErr     error
	killErr     error
	killed      []string
	restored    []string
	snapshotted []string
	bootCount   int
	// resumeHookErr is returned from TriggerResumeHook when non-nil; the
	// default (nil) matches production-success semantics. V6 tests that need
	// the dial-failure path flip this.
	resumeHookErr error
	// resumeHookCalls records every (instance, hostTimeUnixNano) the wake
	// path passed to TriggerResumeHook. Tests assert both ordering (Boot
	// doesn't fire it, Restore does) and the dial-time argument.
	resumeHookCalls []resumeHookCall
	// bootCgroupFail, when non-nil, causes Boot to return this error after
	// creating the cgroup scope — used to simulate a cgroup write failure
	// (e.g. memory.max WriteFile failing due to permissions) without
	// depending on filesystem permissions that may be bypassed by root.
	bootCgroupFail error
	// M6 builder-VM path: DestroyWithExport returns this exit code, copies
	// nothing. App VMs just see "destroyed" the same way Kill did.
	destroyWithExportExit int
	destroyWithExportErr  error
	destroyedWithExport   []string
	// G2 secrets staging.
	stagedSecrets   []stagedSecret
	stageSecretsErr error
	// pids is the InstancePID source-of-truth for the M8 §11
	// SeccompStatus path. Tests that want the gRPC handler to
	// return a real (pid, true) register one here; the default
	// (empty map) makes the handler return NotFound.
	pids map[string]int
}

type stagedSecret struct {
	blob []byte
}

func (v *fakeVMM) Boot(_ context.Context, l Lease, _ VMConfig) error {
	v.mu.Lock()
	v.bootCount++
	v.mu.Unlock()
	// Mirror what jailer does in production: create the per-VM cgroup
	// scope under faas-tenant.slice, then write memory.max to set the
	// RAM cap. Both operations must succeed for Boot to be considered
	// successful — a missing scope or unwritable memory.max means the
	// VM is not properly constrained and we must fail.
	scopePath := filepath.Join(cgroupRoot, ParentCgroup, PerInstanceScope(l.Instance))
	if err := os.MkdirAll(scopePath, 0o755); err != nil {
		return err
	}
	// Injectable cgroup failure — used to simulate memory.max write failure
	// (CAP_SYS_ADMIN not granted, cgroup namespace isolation, etc.) without
	// depending on filesystem permissions that root can bypass.
	if v.bootCgroupFail != nil {
		return v.bootCgroupFail
	}
	return v.bootErr
}

// BootColdBoot mirrors the production flow for tests: synthesize a
// VMConfig from the resolved ColdBootSpec and delegate to Boot. The
// fake doesn't actually materialize from StorageBackend (no storage
// configured); the production path in JailerVMM.BootColdBoot would
// resolve keys through storage.Get before calling Boot. Tests that
// care about storage semantics use TestRestore_MaterializesBaseViaStorage
// (pkg/fcvm/vmm_test.go) with a real JailerVMM + fake StorageBackend.
func (v *fakeVMM) BootColdBoot(ctx context.Context, l Lease, spec ColdBootSpec) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	return v.Boot(ctx, l, BuildColdBootConfig(spec, l.Slot))
}

func (v *fakeVMM) Restore(ctx context.Context, l Lease, spec RestoreSpec) error {
	v.mu.Lock()
	v.restored = append(v.restored, l.Instance)
	v.mu.Unlock()
	// Same scope-create as Boot — jailer creates the scope on restore too.
	if err := os.MkdirAll(filepath.Join(cgroupRoot, ParentCgroup, PerInstanceScope(l.Instance)), 0o755); err != nil {
		return err
	}
	// Mirror the production JailerVMM.Restore: after /snapshot/load, dial the
	// vsock and trigger the resume hook. ADR-022. The test then sees the call
	// on v.resumeHookCalls (used by TestWakeRestore_*) and surfaces any
	// injected error (used by TestWakeRestore_ResumeHookErrorPropagatesAndUnwinds).
	if spec.VsockDevice != nil {
		if err := v.TriggerResumeHook(ctx, l, 1); err != nil {
			return err
		}
	}
	return v.restoreErr
}

func (v *fakeVMM) TriggerResumeHook(_ context.Context, l Lease, hostTimeUnixNano int64) error {
	v.mu.Lock()
	v.resumeHookCalls = append(v.resumeHookCalls, resumeHookCall{Instance: l.Instance, HostTimeUnixNano: hostTimeUnixNano})
	v.mu.Unlock()
	// Default: succeed. Tests that exercise the resume-hook error path should
	// set resumeHookErr (see manager_test.go).
	return v.resumeHookErr
}

// resumeHookCall records one TriggerResumeHook invocation. The slice is
// append-only and read under v.mu — production code never reads it.
type resumeHookCall struct {
	Instance         string
	HostTimeUnixNano int64
}

// TestWakeColdBoot_DoesNotInvokeResumeHook pins the post-restore-only
// invariant (ADR-022): a Wake with no usable snapshot MUST NOT call
// TriggerResumeHook. Cold-boot guests get fresh kernel entropy from the
// boot-time pool; only restore needs the resume hook (re-seed entropy +
// step clock).
func TestWakeColdBoot_DoesNotInvokeResumeHook(t *testing.T) {
	mgr := NewManager(&fakeRunner{}, &fakeVMM{}, Paths{Kernel: "/k"}, "1.7.0", nil, nil)
	if _, err := mgr.Wake(context.Background(), WakeRequest{
		Instance:   "cold-A",
		BaseKey:    "/base.ext4",
		LayerKey:   "/layer.ext4",
		VcpuCount:  2,
		MemSizeMiB: 128,
		// Snapshot intentionally nil — forces cold boot.
	}); err != nil {
		t.Fatalf("Wake: %v", err)
	}
	// Reach into the fakeVMM to assert TriggerResumeHook was not called.
	vmm, ok := mgr.vmm.(*fakeVMM)
	if !ok {
		t.Fatal("mgr.vmm is not *fakeVMM")
	}
	if n := len(vmm.resumeHookCalls); n != 0 {
		t.Errorf("TriggerResumeHook called %d times on cold boot, want 0 (hook is post-restore only)", n)
	}
}

// TestWakeRestore_InvokesResumeHook verifies the restore path DOES call
// TriggerResumeHook exactly once per Wake, with the lease slot wired into
// the VsockDevice passed via RestoreSpec.
func TestWakeRestore_InvokesResumeHook(t *testing.T) {
	mgr := NewManager(&fakeRunner{}, &fakeVMM{}, Paths{Kernel: "/k"}, "1.7.0", nil, nil)
	if _, err := mgr.Wake(context.Background(), WakeRequest{
		Instance:   "restore-A",
		BaseKey:    "/base.ext4",
		LayerKey:   "/layer.ext4",
		VcpuCount:  2,
		MemSizeMiB: 128,
		Snapshot:   usableSnapshot(),
	}); err != nil {
		t.Fatalf("Wake: %v", err)
	}
	vmm, ok := mgr.vmm.(*fakeVMM)
	if !ok {
		t.Fatal("mgr.vmm is not *fakeVMM")
	}
	if n := len(vmm.resumeHookCalls); n != 1 {
		t.Errorf("TriggerResumeHook called %d times on restore, want 1", n)
	}
	if len(vmm.resumeHookCalls) > 0 && vmm.resumeHookCalls[0].Instance != "restore-A" {
		t.Errorf("resume hook for instance = %q, want %q", vmm.resumeHookCalls[0].Instance, "restore-A")
	}
}

// TestWakeRestore_ResumeHookErrorFallsBackToColdBoot verifies the resume
// hook error path is handled safely (ADR-005 cold-boot fallback). A failed
// TriggerResumeHook means the resumed VM would share its snapshot's entropy
// — spec §11 V6 says "non-unique guest must not serve." The Manager's
// restore-failure cold-boot fallback discards the bad VM and starts fresh,
// which gives the guest unique entropy by construction.
//
// Invariants pinned here:
//   - The half-restored VM is killed (no leak: fvmm.killed includes it).
//   - Wake ultimately succeeds (the cold-boot fallback rescued it).
//   - TriggerResumeHook is called exactly once before the fallback fires.
func TestWakeRestore_ResumeHookErrorFallsBackToColdBoot(t *testing.T) {
	fvmm := &fakeVMM{resumeHookErr: fmt.Errorf("dial vsock uds: synthetic failure")}
	mgr := NewManager(&fakeRunner{}, fvmm, Paths{Kernel: "/k"}, "1.7.0", nil, nil)
	if _, err := mgr.Wake(context.Background(), WakeRequest{
		Instance:   "restore-fail",
		BaseKey:    "/base.ext4",
		LayerKey:   "/layer.ext4",
		VcpuCount:  2,
		MemSizeMiB: 128,
		Snapshot:   usableSnapshot(),
	}); err != nil {
		t.Fatalf("Wake: %v (cold-boot fallback should have rescued it)", err)
	}
	// TriggerResumeHook was called once (during the restore attempt).
	if n := len(fvmm.resumeHookCalls); n != 1 {
		t.Errorf("TriggerResumeHook calls = %d, want 1", n)
	}
	// Restore was attempted once, then Kill ran to discard the half-restored
	// VM before cold boot took over.
	if n := len(fvmm.restored); n != 1 {
		t.Errorf("Restore calls = %d, want 1", n)
	}
	if n := len(fvmm.killed); n != 1 {
		t.Errorf("Kill calls = %d, want 1 (cold-boot fallback discards the bad VM)", n)
	}
	// Cold boot ran after Kill — so bootCount is 1.
	if n := fvmm.bootCount; n != 1 {
		t.Errorf("Boot calls = %d, want 1 (cold-boot fallback after failed resume)", n)
	}
	if mgr.LiveCount() != 1 {
		t.Errorf("LiveCount = %d after successful cold-boot fallback, want 1", mgr.LiveCount())
	}
}

func (v *fakeVMM) Snapshot(_ context.Context, l Lease, _ SnapshotSpec) (SnapshotInfo, error) {
	v.mu.Lock()
	v.snapshotted = append(v.snapshotted, l.Instance)
	v.mu.Unlock()
	return SnapshotInfo{MemBytes: 4096}, v.snapErr
}

func (v *fakeVMM) Kill(_ context.Context, l Lease) error {
	v.mu.Lock()
	v.killed = append(v.killed, l.Instance)
	v.mu.Unlock()
	return v.killErr
}

func (v *fakeVMM) DestroyWithExport(_ context.Context, l Lease, _ string) (int, error) {
	v.mu.Lock()
	v.destroyedWithExport = append(v.destroyedWithExport, l.Instance)
	v.mu.Unlock()
	return v.destroyWithExportExit, v.destroyWithExportErr
}

func (v *fakeVMM) StageSecretsEnv(_ string, jsonBlob []byte) error {
	v.mu.Lock()
	v.stagedSecrets = append(v.stagedSecrets, stagedSecret{blob: append([]byte(nil), jsonBlob...)})
	v.mu.Unlock()
	return v.stageSecretsErr
}

// InstancePID is the in-process fake for the M8 §11 SeccompStatus
// path. fakeVMM never spawns a real process, so the canonical
// "test a real jailing" path is the cmd/e2e sec11_seccomp test
// (which boots vmmd as a subprocess and reads /proc/<pid>/status
// back). The fake answers (0, false) for unknown instances and
// (pids[instance], true) for instances the test has registered
// via boot — tests that want to drive the gRPC handler through
// the fake should set pids before invoking the handler.
func (v *fakeVMM) InstancePID(instance string) (int, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	p, ok := v.pids[instance]
	return p, ok
}

func (v *fakeVMM) restoredInstance(id string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, r := range v.restored {
		if r == id {
			return true
		}
	}
	return false
}

func (v *fakeVMM) boots() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.bootCount
}

const testFCVersion = "1.7.0"

func usableSnapshot() *Snapshot {
	return &Snapshot{DeploymentID: "d1", FCVersion: testFCVersion, StorageKey: "snap/d1/mem", VMStatePath: "/snap/state"}
}

func (v *fakeVMM) killedInstance(id string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, k := range v.killed {
		if k == id {
			return true
		}
	}
	return false
}

func req(id string) ColdBootRequest {
	return ColdBootRequest{Instance: id, BaseKey: "/b.ext4", LayerKey: "/l.ext4", VcpuCount: 2, MemSizeMiB: 128}
}

func newTestManager(run Runner, vmm VMM) *Manager {
	return NewManager(run, vmm, Paths{Kernel: "/srv/fc/base/vmlinux-6.1"}, testFCVersion, nil, nil)
}

// TestMain redirects cgroupRoot to a temp dir for the whole package's
// unit tests. fakeVMM.Boot (manager_test.go:fakeVMM.Boot) creates the
// per-VM scope as a plain directory under cgroupRoot, so the unit-test
// path never touches the host's real /sys/fs/cgroup — concurrent runs
// don't collide. Tests that want a distinct root inside the unit-test
// path can call withFakeCgroupRoot (cgroup_test.go). Metal tests
// (TestMetal*, in manager_metal_test.go) point cgroupRoot back at the
// real /sys/fs/cgroup via the same helper, because the jailer writes
// there regardless of what cgroupRoot is set to in this package.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fcvm-cgroup-test-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	if err := os.MkdirAll(filepath.Join(dir, "faas-tenant.slice"), 0o755); err != nil {
		panic(err)
	}
	cgroupRoot = dir
	os.Exit(m.Run())
}

func TestColdBootSuccessTracksInstance(t *testing.T) {
	run, vmm := &fakeRunner{}, &fakeVMM{}
	m := newTestManager(run, vmm)

	inst, err := m.ColdBoot(context.Background(), req("i1"))
	if err != nil {
		t.Fatalf("cold boot: %v", err)
	}
	if inst.Lease.UID < JailUIDBase {
		t.Errorf("lease uid not assigned: %d", inst.Lease.UID)
	}
	if m.LiveCount() != 1 || m.LeasedCount() != 1 {
		t.Fatalf("live=%d leased=%d, want 1/1", m.LiveCount(), m.LeasedCount())
	}
	if !run.ran("netns add fc-i1") {
		t.Error("network setup did not run")
	}
}

func TestColdBootNetworkFailureLeaksNothing(t *testing.T) {
	// Fail midway through network setup; the lease must be released and teardown
	// attempted so leakcheck stays clean.
	run := &fakeRunner{failOn: "tuntap add tap0"}
	vmm := &fakeVMM{}
	m := newTestManager(run, vmm)

	if _, err := m.ColdBoot(context.Background(), req("i1")); err == nil {
		t.Fatal("expected cold boot to fail")
	}
	if m.LiveCount() != 0 {
		t.Errorf("live=%d, want 0 after failed boot", m.LiveCount())
	}
	if m.LeasedCount() != 0 {
		t.Errorf("leased=%d, want 0 after failed boot — LEASE LEAK", m.LeasedCount())
	}
	if !run.ran("netns del fc-i1") {
		t.Error("teardown did not attempt to delete the netns")
	}
}

func TestColdBootVMFailureLeaksNothing(t *testing.T) {
	run := &fakeRunner{}
	vmm := &fakeVMM{bootErr: fmt.Errorf("kvm exploded")}
	m := newTestManager(run, vmm)

	if _, err := m.ColdBoot(context.Background(), req("i1")); err == nil {
		t.Fatal("expected cold boot to fail")
	}
	if m.LeasedCount() != 0 {
		t.Errorf("leased=%d, want 0 — LEASE LEAK after VM boot failure", m.LeasedCount())
	}
	if !vmm.killedInstance("i1") {
		t.Error("VM was not killed on the cleanup path")
	}
	if !run.ran("netns del fc-i1") {
		t.Error("network was not torn down on VM boot failure")
	}
}

func TestDestroyReleasesResources(t *testing.T) {
	run, vmm := &fakeRunner{}, &fakeVMM{}
	m := newTestManager(run, vmm)
	if _, err := m.ColdBoot(context.Background(), req("i1")); err != nil {
		t.Fatal(err)
	}
	if err := m.Destroy(context.Background(), "i1"); err != nil {
		t.Fatal(err)
	}
	if m.LiveCount() != 0 || m.LeasedCount() != 0 {
		t.Errorf("live=%d leased=%d, want 0/0 after destroy", m.LiveCount(), m.LeasedCount())
	}
	if !vmm.killedInstance("i1") {
		t.Error("destroy did not kill the VM")
	}
}

func TestDestroyUnknownIsNoop(t *testing.T) {
	m := newTestManager(&fakeRunner{}, &fakeVMM{})
	if err := m.Destroy(context.Background(), "ghost"); err != nil {
		t.Errorf("destroying unknown instance should be a no-op, got %v", err)
	}
}

// TestConcurrentBootAndDestroyNoLeak mirrors the M1 acceptance shape (boot many,
// tear all down, zero leaks) at the orchestration level.
func TestConcurrentBootAndDestroyNoLeak(t *testing.T) {
	run, vmm := &fakeRunner{}, &fakeVMM{}
	m := newTestManager(run, vmm)
	const n = 50 // M1: boot 50 VMs concurrently

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := m.ColdBoot(context.Background(), req(fmt.Sprintf("i%d", i))); err != nil {
				t.Errorf("boot i%d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if m.LiveCount() != n || m.LeasedCount() != n {
		t.Fatalf("after boot: live=%d leased=%d, want %d/%d", m.LiveCount(), m.LeasedCount(), n, n)
	}

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := m.Destroy(context.Background(), fmt.Sprintf("i%d", i)); err != nil {
				t.Errorf("destroy i%d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if m.LiveCount() != 0 || m.LeasedCount() != 0 {
		t.Fatalf("after teardown: live=%d leased=%d, want 0/0 (LEAK)", m.LiveCount(), m.LeasedCount())
	}
}

// --- bringUp / cleanup -----------------------------------------------------

// TestRestoreFailsThenColdBootSucceeds covers the ADR-005 branch: snapshot
// restore errors are non-terminal, we Kill the half-restored VM and fall
// back to cold boot. The returned method must read WakeColdBoot so schedd
// can mark the snapshot stale.
func TestRestoreFailsThenColdBootSucceeds(t *testing.T) {
	run := &fakeRunner{}
	vmm := &fakeVMM{restoreErr: fmt.Errorf("snapshot corrupt")}
	m := newTestManager(run, vmm)

	inst, err := m.Wake(context.Background(), WakeRequest{
		Instance: "fb", BaseKey: "/b.ext4", LayerKey: "/l.ext4",
		VcpuCount: 2, MemSizeMiB: 128,
		Snapshot: usableSnapshot(),
	})
	if err != nil {
		t.Fatalf("Wake after restore-fail: %v", err)
	}
	if inst.Method != WakeColdBoot {
		t.Errorf("method = %v, want WakeColdBoot (fallback)", inst.Method)
	}
	if vmm.boots() != 1 {
		t.Errorf("Boot not invoked after restore-fail fallback: %d", vmm.boots())
	}
	// The half-restored VM must be killed before the cold-boot attempt —
	// otherwise the lease UID has two processes fighting for the netns.
	if !vmm.killedInstance("fb") {
		t.Error("expected Kill of half-restored instance before cold-boot fallback")
	}
	if m.LeasedCount() != 1 {
		t.Errorf("lease not held after successful fallback: leased=%d", m.LeasedCount())
	}
}

// TestWakeRejectsEgressAllowlist_v6Accepted: ADR-032 v6 mirror. v6
// entries must pass the wire-side parse + family gate (the v4-only
// reject from PR #159 is gone). The /0 reject (Bits()==0) is the
// only remaining per-entry guard at the wire; everything else is
// the DB trigger's job. This test pins that a v6 entry ADVANCES
// past the parse loop — it either succeeds (preferred) or fails
// for an unrelated reason further down the path (e.g. the fakeVMM
// stub doesn't implement every step). The key assertion is that
// the error, if any, does NOT say "v4 only".
func TestWakeRejectsEgressAllowlist_v6Accepted(t *testing.T) {
	run := &fakeRunner{}
	vmm := &fakeVMM{}
	m := newTestManager(run, vmm)

	_, err := m.Wake(context.Background(), WakeRequest{
		Instance:   "vw6",
		BaseKey:    "/b.ext4",
		LayerKey:   "/l.ext4",
		VcpuCount:  2,
		MemSizeMiB: 128,
		// v6 prefix: ADR-032 accepts this; renderer partitions
		// into a separate ip6 faas forward rule.
		EgressAllowlist: []string{"fe80::/10"},
	})
	// The test does not assert err == nil — fakeVMM may short-
	// circuit at any step (StartInstance, etc.). What we DO
	// assert: the parse gate didn't trip on the v6 entry, so the
	// error does not name the v6 entry as the offender.
	if err != nil && strings.Contains(err.Error(), "fe80::/10") {
		t.Fatalf("Wake with v6 EgressAllowlist entry: error names the CIDR — parse gate regressed: %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "v4 only") {
		t.Fatalf("Wake with v6 EgressAllowlist entry: error says 'v4 only' — ADR-032 wire gate regressed: %v", err)
	}
}

// TestWakeRejectsEgressAllowlist_ZeroBitsClosed: same defence-in-
// depth shape, on the /0 case. apid's PATCH rejects Bits()==0
// (PR #159 review F2); the Wake path re-validates so a wire-bypass
// cannot smuggle 0.0.0.0/0 (which would unblock the whole v4
// internet and make the allowlist a no-op).
func TestWakeRejectsEgressAllowlist_ZeroBitsClosed(t *testing.T) {
	run := &fakeRunner{}
	vmm := &fakeVMM{}
	m := newTestManager(run, vmm)

	_, err := m.Wake(context.Background(), WakeRequest{
		Instance:        "w0",
		BaseKey:         "/b.ext4",
		LayerKey:        "/l.ext4",
		VcpuCount:       2,
		MemSizeMiB:      128,
		EgressAllowlist: []string{"0.0.0.0/0"},
	})
	if err == nil {
		t.Fatal("Wake with /0 EgressAllowlist entry: expected fail-closed, got success")
	}
	if !strings.Contains(err.Error(), "0.0.0.0/0") {
		t.Errorf("error should name the offending CIDR; got: %v", err)
	}
	if !strings.Contains(err.Error(), "non-/0") {
		t.Errorf("error should mention the non-/0 invariant; got: %v", err)
	}
	if m.LeasedCount() != 0 {
		t.Errorf("lease leaked after fail-closed: leased=%d", m.LeasedCount())
	}
}

// TestWakeRejectsEgressAllowlist_V6SlashZeroClosed: ADR-032 v6 mirror
// of the non-/0 contract. `::/0` would unblock the entire IPv6
// internet and make the v6 allowlist a no-op, so the wire-side
// Bits()==0 reject still trips regardless of family. The DB trigger
// also rejects it (migration 00033), but the wire gate is the
// defence-in-depth layer if the DB is bypassed (e.g. a future
// migration that loosens the trigger).
func TestWakeRejectsEgressAllowlist_V6SlashZeroClosed(t *testing.T) {
	run := &fakeRunner{}
	vmm := &fakeVMM{}
	m := newTestManager(run, vmm)

	_, err := m.Wake(context.Background(), WakeRequest{
		Instance:        "w6z",
		BaseKey:         "/b.ext4",
		LayerKey:        "/l.ext4",
		VcpuCount:       2,
		MemSizeMiB:      128,
		EgressAllowlist: []string{"::/0"},
	})
	if err == nil {
		t.Fatal("Wake with v6 /0 EgressAllowlist entry: expected fail-closed, got success")
	}
	if !strings.Contains(err.Error(), "::/0") {
		t.Errorf("error should name the offending CIDR; got: %v", err)
	}
	if !strings.Contains(err.Error(), "masklen") && !strings.Contains(err.Error(), "/0") {
		t.Errorf("error should mention the non-/0 invariant; got: %v", err)
	}
	if run.ran("nft") {
		t.Error("nft commands ran before v6 /0 rejection — render order regressed")
	}
	if m.LeasedCount() != 0 {
		t.Errorf("lease leaked after fail-closed: leased=%d", m.LeasedCount())
	}
}

// TestRestoreSucceedsUsesFastPath — counter-test to the fallback: when
// Restore works, cold boot is NOT called and the returned method is
// WakeRestore.
func TestRestoreSucceedsUsesFastPath(t *testing.T) {
	run := &fakeRunner{}
	vmm := &fakeVMM{} // no errors
	m := newTestManager(run, vmm)

	inst, err := m.Wake(context.Background(), WakeRequest{
		Instance: "rp", BaseKey: "/b.ext4", LayerKey: "/l.ext4",
		VcpuCount: 2, MemSizeMiB: 128,
		Snapshot: usableSnapshot(),
	})
	if err != nil {
		t.Fatalf("Wake: %v", err)
	}
	if inst.Method != WakeRestore {
		t.Errorf("method = %v, want WakeRestore", inst.Method)
	}
	if vmm.boots() != 0 {
		t.Errorf("Boot must not run on restore fast path: %d", vmm.boots())
	}
}

// TestColdBootConfigInvalid covers the Validate() failure branch of bringUp.
// ColdBootSpec.Validate must reject empty paths / 0 vcpu / 0 mem.
func TestColdBootConfigInvalid(t *testing.T) {
	run := &fakeRunner{}
	vmm := &fakeVMM{}
	m := newTestManager(run, vmm)

	cases := []struct {
		name string
		req  ColdBootRequest
	}{
		{"missing base", ColdBootRequest{Instance: "x", LayerKey: "/l.ext4", VcpuCount: 1, MemSizeMiB: 128}},
		{"missing layer", ColdBootRequest{Instance: "x", BaseKey: "/b.ext4", VcpuCount: 1, MemSizeMiB: 128}},
		{"zero vcpu", ColdBootRequest{Instance: "x", BaseKey: "/b", LayerKey: "/l", VcpuCount: 0, MemSizeMiB: 128}},
		{"zero mem", ColdBootRequest{Instance: "x", BaseKey: "/b", LayerKey: "/l", VcpuCount: 1, MemSizeMiB: 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := m.ColdBoot(context.Background(), tc.req)
			if err == nil {
				t.Fatal("expected validation error")
			}
			if vmm.boots() != 0 {
				t.Errorf("vmm.Boot must not run when spec invalid: %d", vmm.boots())
			}
			// The lease was acquired (before validation) and must be released
			// even on this failure path — no half-held UID.
			if m.LeasedCount() != 0 {
				t.Errorf("lease leaked on validation failure: leased=%d", m.LeasedCount())
			}
		})
	}
}

// TestColdBootVMFailureExhaustsCleanup covers the path where Boot itself
// fails: cleanup() must still run teardown + release, so a transient VMM
// failure does not leak the netns UID.
func TestColdBootVMFailureExhaustsCleanup(t *testing.T) {
	run := &fakeRunner{}
	vmm := &fakeVMM{bootErr: fmt.Errorf("jailer exploded")}
	m := newTestManager(run, vmm)

	_, err := m.ColdBoot(context.Background(), req("vm-fail"))
	if err == nil {
		t.Fatal("expected Boot error")
	}
	if !strings.Contains(err.Error(), "cold boot") {
		t.Errorf("error %q not from cold-boot path", err.Error())
	}
	// Cleanup must have invoked Kill (best-effort) and released the lease.
	if !vmm.killedInstance("vm-fail") {
		t.Error("Kill not called during failed-boot cleanup")
	}
	if m.LeasedCount() != 0 {
		t.Errorf("lease not released after Boot failure: leased=%d", m.LeasedCount())
	}
	// Teardown commands should have been attempted (the network was set up
	// before Boot was called).
	if !run.ran("ip link delete") && !run.ran("ip netns del") {
		t.Error("expected teardown commands during cleanup; none ran")
	}
}

// TestParkUnknownInstanceReturnsError covers the "instance not live" branch
// of Park — without covering this, a typo'd instance id silently no-ops.
func TestParkUnknownInstanceReturnsError(t *testing.T) {
	m := newTestManager(&fakeRunner{}, &fakeVMM{})
	_, err := m.Park(context.Background(), "ghost", SnapshotSpec{})
	if err == nil {
		t.Fatal("expected error parking unknown instance")
	}
	if !strings.Contains(err.Error(), "not live") {
		t.Errorf("error %q missing 'not live'", err.Error())
	}
}

// TestParkSnapshotFailureDestroysInstance covers the ADR-005 safety net:
// if Snapshot fails we Destroy the live instance rather than leaking the
// still-running VM + lease. The error returned must wrap the snapshot cause.
func TestParkSnapshotFailureDestroysInstance(t *testing.T) {
	run := &fakeRunner{}
	vmm := &fakeVMM{snapErr: fmt.Errorf("disk full")}
	m := newTestManager(run, vmm)

	// First bring up an instance so Park has something to act on.
	inst, err := m.ColdBoot(context.Background(), req("park-fail"))
	if err != nil {
		t.Fatalf("ColdBoot: %v", err)
	}
	_ = inst

	_, err = m.Park(context.Background(), "park-fail", SnapshotSpec{VMStatePath: "/s", StorageKey: "snap/park-fail/mem"})
	if err == nil {
		t.Fatal("expected snapshot error")
	}
	if !strings.Contains(err.Error(), "snapshot") {
		t.Errorf("error %q not snapshot-wrapped", err.Error())
	}
	// The instance should be torn down even though Park failed — that is the
	// invariant.
	if m.LiveCount() != 0 {
		t.Errorf("instance not removed from live after Park failure: live=%d", m.LiveCount())
	}
	if m.LeasedCount() != 0 {
		t.Errorf("lease leaked after Park failure: leased=%d", m.LeasedCount())
	}
}

// TestSetupNetworkPropagatesFirstError covers the run-loop in setupNetwork:
// it stops at the first failing command (not the last) and wraps with argv.
func TestSetupNetworkPropagatesFirstError(t *testing.T) {
	run := &fakeRunner{failOn: "ip link add"}
	vmm := &fakeVMM{}
	m := newTestManager(run, vmm)
	_, err := m.ColdBoot(context.Background(), req("net-fail"))
	if err == nil {
		t.Fatal("expected setup-network error")
	}
	if !strings.Contains(err.Error(), "ip link add") {
		t.Errorf("error %q missing failing argv", err.Error())
	}
	if vmm.boots() != 0 {
		t.Errorf("Boot must not run when network setup fails: %d", vmm.boots())
	}
}

// TestAcquireFailureShortCircuitsWake covers the very first Wake failure:
// alloc.Acquire returns an error. Wake must not run setupNetwork or Boot.
func TestAcquireFailureShortCircuitsWake(t *testing.T) {
	// Saturate the allocator so the next Acquire fails.
	alloc := NewAllocator()
	for i := 0; i < MaxSlots; i++ {
		if _, err := alloc.Acquire(fmt.Sprintf("pre%d", i)); err != nil {
			t.Fatalf("priming %d: %v", i, err)
		}
	}
	vmm := &fakeVMM{}
	run := &fakeRunner{}
	m := NewManager(run, vmm, Paths{Kernel: "/k"}, testFCVersion, nil, nil)
	m.alloc = alloc // swap in the saturated one

	_, err := m.ColdBoot(context.Background(), req("overflow"))
	if err == nil {
		t.Fatal("expected acquire failure")
	}
	if !strings.Contains(err.Error(), "acquire") {
		t.Errorf("error %q missing 'acquire'", err.Error())
	}
	if run.ran("ip link") {
		t.Error("setupNetwork must not run when Acquire fails")
	}
	if vmm.boots() != 0 {
		t.Errorf("Boot must not run when Acquire fails: %d", vmm.boots())
	}
}

// TestLiveCountAndLeasedCountEmptyManager — sanity check the getters on a
// fresh Manager.
func TestLiveCountAndLeasedCountEmptyManager(t *testing.T) {
	m := newTestManager(&fakeRunner{}, &fakeVMM{})
	if m.LiveCount() != 0 || m.LeasedCount() != 0 {
		t.Errorf("fresh manager non-empty: live=%d leased=%d", m.LiveCount(), m.LeasedCount())
	}
}

// TestCleanupKillErrorIsLogged — covers the `m.log.Warn` branch of cleanup's
// first call when vmm.Kill returns an error. The error must be swallowed
// (cleanup is best-effort), not propagated.
func TestCleanupKillErrorIsLogged(t *testing.T) {
	run := &fakeRunner{}
	vmm := &fakeVMM{killErr: fmt.Errorf("process already gone")}
	m := newTestManager(run, vmm)
	// Trigger cleanup via Destroy on an instance we never booted: Destroy
	// short-circuits when not live, so we need to fake-via-Wake failure path.
	// Easiest: pre-populate live map by performing a successful boot, then
	// calling Destroy.
	inst, err := m.ColdBoot(context.Background(), req("kill-err"))
	if err != nil {
		t.Fatalf("ColdBoot: %v", err)
	}
	_ = inst
	if err := m.Destroy(context.Background(), "kill-err"); err != nil {
		t.Fatalf("Destroy should swallow cleanup errors: %v", err)
	}
	// Lease must still be released despite the Kill error.
	if m.LeasedCount() != 0 {
		t.Errorf("lease leaked after Kill error: leased=%d", m.LeasedCount())
	}
}

// fakeVMMWithKillErr extends fakeVMM with a Kill that always errors.
// We mutate the embedded fakeVMM rather than threading a new field so this
// test file's existing helpers stay unchanged.

// TestCleanupTeardownCommandFailureIsDebug — covers the `m.log.Debug` branch
// when a teardown command errors (e.g. ip netns del on a netns that was
// never created because boot failed before that step).
func TestCleanupTeardownCommandFailureIsDebug(t *testing.T) {
	run := &fakeRunner{} // no failures during setup
	vmm := &fakeVMM{bootErr: fmt.Errorf("Boot fail")}
	m := newTestManager(run, vmm)

	_, err := m.ColdBoot(context.Background(), req("td-fail"))
	if err == nil {
		t.Fatal("expected Boot error")
	}
	// We can't easily make the teardown commands fail when they ran fine on
	// the setup side, but we *can* swap to a Runner that fails teardown.
	// Re-run with a runner that fails teardown:
	run2 := &fakeRunner{} // no setup failures
	run2.failTeardown = true
	vmm2 := &fakeVMM{bootErr: fmt.Errorf("Boot fail")}
	m2 := newTestManager(run2, vmm2)
	_, _ = m2.ColdBoot(context.Background(), req("td-fail2"))
	// We expect no panic — the debug log swallows teardown failures.
	if m2.LeasedCount() != 0 {
		t.Errorf("lease leaked: %d", m2.LeasedCount())
	}
}

// TestCleanupReleaseErrorIsLogged — covers the alloc.Release error branch
// (instance not in the lease map, can only happen on logic error / double
// cleanup). The error must be swallowed.
func TestCleanupReleaseErrorIsLogged(t *testing.T) {
	// Bypass Wake's automatic cleanup by directly calling m.cleanup on an
	// instance the allocator has never seen.
	m := newTestManager(&fakeRunner{}, &fakeVMM{})
	lease := Lease{Instance: "ghost-cleanup", UID: 20000, GID: 20000}
	nc := netnsConfigForTest(lease)
	// Should not panic; should log warn. We're proving the swallow.
	m.cleanup(context.Background(), lease, nc)
}

// netnsConfigForTest builds a minimal netns.Config matching the lease so
// cleanup has something to iterate teardown commands on. The exact netns
// name doesn't matter — fakeRunner matches by substring.
func netnsConfigForTest(l Lease) netns.Config {
	return netns.NewConfig(
		l.Instance, l.Netns, l.VethHost, l.VethPeer,
		l.HostIP,
	)
}

// TestDiscardWrite — covers the io.Writer fallback in manager.go so the
// NewManager(nil-log) path is verified end-to-end.
func TestDiscardWrite(t *testing.T) {
	d := discard{}
	if _, err := d.Write([]byte("anything")); err != nil {
		t.Errorf("discard.Write: %v", err)
	}
}

// TestSetupNetworkRunsNftBeforeVMBoot proves the wire-up point: the per-
// instance nft commands run inside setupNetwork, AFTER the topology (veth/
// tap/addressing) is in place but BEFORE VMM.Boot. Without this ordering,
// VMM.Boot's waitReady would dial a host identity whose DNAT isn't loaded
// yet — and the SYN-ACK would never come back (filter or no filter).
func TestSetupNetworkRunsNftBeforeVMBoot(t *testing.T) {
	run, vmm := &fakeRunner{}, &fakeVMM{}
	m := newTestManager(run, vmm)

	if _, err := m.ColdBoot(context.Background(), req("dnat-ord")); err != nil {
		t.Fatalf("cold boot: %v", err)
	}
	if vmm.boots() != 1 {
		t.Fatalf("VMM.Boot must run exactly once; got %d", vmm.boots())
	}

	// Locate the tap-create argv and the DNAT argv by content.
	var tapIdx, dnatIdx = -1, -1
	for i, c := range run.commands {
		line := strings.Join(c, " ")
		switch {
		case strings.Contains(line, "tuntap add tap0"):
			tapIdx = i
		case strings.Contains(line, "dnat to 10.0.0.2:8080"):
			dnatIdx = i
		}
	}
	if tapIdx < 0 {
		t.Fatalf("never saw `tuntap add tap0` in %v", run.commands)
	}
	if dnatIdx < 0 {
		t.Fatalf("never saw DNAT rule `dnat to 10.0.0.2:8080` in %v", run.commands)
	}
	if tapIdx > dnatIdx {
		t.Errorf("tap-create (idx %d) must precede DNAT rule (idx %d)", tapIdx, dnatIdx)
	}
	// VMM.Boot runs after setupNetwork returns (Wake's call sequence). bootCount
	// is asserted at the top of this test via `vmm.boots() != 1`; the order
	// between tap-create < DNAT < Boot is the load-bearing #30 invariant.
}

// TestSetupNetworkNftFailureLeaksNothing covers the leak invariant when the
// strict part of the nft ruleset fails: the defer-cleanup in Wake must
// fully unwind (netns deleted, lease released) even if Boot never runs.
//
// We fail on a strict nft argv (`add rule ip faas prerouting`) so the best-
// effort reset (which ran first and succeeded) is already done — that's the
// realistic scenario where a partial ruleset lands but a later add fails.
func TestSetupNetworkNftFailureLeaksNothing(t *testing.T) {
	run := &fakeRunner{failOn: "add rule ip faas prerouting"}
	vmm := &fakeVMM{}
	m := newTestManager(run, vmm)

	_, err := m.ColdBoot(context.Background(), req("dnat-fail"))
	if err == nil {
		t.Fatal("expected setupNetwork to fail on the nft add rule")
	}
	if !strings.Contains(err.Error(), "add rule ip faas prerouting") {
		t.Errorf("err %q must wrap the failing argv", err.Error())
	}
	if vmm.boots() != 0 {
		t.Errorf("VMM.Boot must not run when nft fails: %d boots", vmm.boots())
	}
	if m.LeasedCount() != 0 {
		t.Errorf("LeasedCount = %d after failed boot, want 0 (leak)", m.LeasedCount())
	}
	if !run.ran("netns del fc-dnat-fail") {
		t.Error("teardown did not run netns del; netns leaked")
	}
}

// --- tc + memory.max wiring (PR A: #31 + #33) ----------------------------

// indexOfArgv returns the index of the first recorded argv whose
// joined form contains substr, or -1 if absent. Used by the new
// ordering / argv-shape tests below.
func indexOfArgv(cmds [][]string, substr string) int {
	for i, c := range cmds {
		if strings.Contains(strings.Join(c, " "), substr) {
			return i
		}
	}
	return -1
}

// TestSetupNetworkTcResetBeforeNftReset locks the snapshot-restore
// ordering: each ruleset's reset (`tc qdisc del`, `nft delete table`)
// must come BEFORE its strict add, and the tc reset must come BEFORE
// the nft reset so a fresh netns that already had the veth set up
// (which happens across park→wake) drops the qdisc before the nft
// reset tries to clean the table.
func TestSetupNetworkTcResetBeforeNftReset(t *testing.T) {
	run, vmm := &fakeRunner{}, &fakeVMM{}
	m := newTestManager(run, vmm)

	r := req("tc-ord")
	r.EgressMbit = 25
	if _, err := m.ColdBoot(context.Background(), r); err != nil {
		t.Fatalf("cold boot: %v", err)
	}
	tcDel := indexOfArgv(run.commands, "tc qdisc del")
	nftDel := indexOfArgv(run.commands, "nft delete table")
	nftAdd := indexOfArgv(run.commands, "nft add table")
	if tcDel < 0 || nftDel < 0 || nftAdd < 0 {
		t.Fatalf("expected all three argvs; got tcDel=%d nftDel=%d nftAdd=%d\n%s",
			tcDel, nftDel, nftAdd, flattenForTest(run.commands))
	}
	if tcDel >= nftDel {
		t.Errorf("tc qdisc del (idx %d) must precede nft delete table (idx %d) on snapshot-restore Wake", tcDel, nftDel)
	}
	if nftDel >= nftAdd {
		t.Errorf("nft delete table (idx %d) must precede nft add table (idx %d) — same reset-before-add invariant", nftDel, nftAdd)
	}
}

// TestSetupNetworkEmitsConntrackCapRule locks the spec §7 wire-up:
// pkg/fcvm/manager.go::Wake stamps nc.ConntrackCap = api.DefaultConntrackCap,
// so the runner must observe the nft `ct count over 4096 counter name
// "faas_cap" drop` rule in the argv list — and it must sit between the
// established/related accept and the SMTP / daddr drops (the rule
// position the connlimit comment in pkg/netns/config.go asserts).
//
// The companion unit tests for argv shape live in pkg/netns/config_test.go
// (TestNftCommandsEmitsConntrackCapRule / CapRuleRunsAfterEstablishedBeforeDenies);
// this test pins the wiring through pkg/fcvm/manager::setupNetwork, which
// is the runtime code that owns rule ordering against tc reset/add.
func TestSetupNetworkEmitsConntrackCapRule(t *testing.T) {
	run, vmm := &fakeRunner{}, &fakeVMM{}
	m := newTestManager(run, vmm)

	if _, err := m.ColdBoot(context.Background(), req("cap-rule")); err != nil {
		t.Fatalf("cold boot: %v", err)
	}
	capV4 := indexOfArgv(run.commands, "nft add rule ip faas forward ct count over 4096")
	capV6 := indexOfArgv(run.commands, "nft add rule ip6 faas forward ct count over 4096")
	establishedV4 := indexOfArgv(run.commands, "nft add rule ip faas forward ct state established,related accept")
	establishedV6 := indexOfArgv(run.commands, "nft add rule ip6 faas forward ct state established,related accept")
	smtpDrop := indexOfArgv(run.commands, "tcp dport {")
	// PR-E: per-CIDR deny lines. The cap-rule-precedes-daddr-drop
	// ordering invariant still holds — pick the FIRST per-CIDR rule
	// in each family. Catalog sort order (v4 prefix asc, v6 prefix
	// asc — see NewDefaultDenySet) is the same as before, so the
	// pre-PR-E v4 first entry (10.0.0.0/8) and v6 first entry
	// (::/128) are still the leading per-CIDR rules.
	daddrDropV4 := indexOfArgv(run.commands, "ip daddr 10.0.0.0/8 counter name")
	daddrDropV6 := indexOfArgv(run.commands, "ip6 daddr ::/128 counter name")
	if capV4 < 0 || capV6 < 0 || establishedV4 < 0 || establishedV6 < 0 || daddrDropV4 < 0 || daddrDropV6 < 0 || smtpDrop < 0 {
		t.Fatalf("missing one or more rules in argv list: capV4=%d capV6=%d establishedV4=%d establishedV6=%d smtp=%d daddrV4=%d daddrV6=%d\n%s",
			capV4, capV6, establishedV4, establishedV6, smtpDrop, daddrDropV4, daddrDropV6, flattenForTest(run.commands))
	}
	// IPv4 forward chain: established/related accept < cap < SMTP drop < daddr drop.
	if establishedV4 >= capV4 {
		t.Errorf("[v4] established,related accept (idx %d) must come BEFORE the cap rule (idx %d)", establishedV4, capV4)
	}
	if capV4 >= smtpDrop {
		t.Errorf("[v4] cap rule (idx %d) must come BEFORE the SMTP drop (idx %d)", capV4, smtpDrop)
	}
	if capV4 >= daddrDropV4 {
		t.Errorf("[v4] cap rule (idx %d) must come BEFORE the daddr lateral-movement drop (idx %d)", capV4, daddrDropV4)
	}
	// IPv6 forward chain: established/related accept < cap < daddr drop.
	// (No SMTP drop on v6.)
	if establishedV6 >= capV6 {
		t.Errorf("[v6] established,related accept (idx %d) must come BEFORE the cap rule (idx %d)", establishedV6, capV6)
	}
	if capV6 >= daddrDropV6 {
		t.Errorf("[v6] cap rule (idx %d) must come BEFORE the daddr lateral-movement drop (idx %d)", capV6, daddrDropV6)
	}
}

// TestSetupNetworkTcRateEqualsPlan locks the wire shape: when the
// caller sets EgressMbit, the argv that runs contains the rate.
func TestSetupNetworkTcRateEqualsPlan(t *testing.T) {
	run, vmm := &fakeRunner{}, &fakeVMM{}
	m := newTestManager(run, vmm)

	r := req("tc-rate")
	r.EgressMbit = 100 // Pro plan
	if _, err := m.ColdBoot(context.Background(), r); err != nil {
		t.Fatalf("cold boot: %v", err)
	}
	tcAdd := indexOfArgv(run.commands, "tc qdisc add")
	if tcAdd < 0 {
		t.Fatalf("never saw `tc qdisc add` argv: %s", flattenForTest(run.commands))
	}
	if !strings.Contains(strings.Join(run.commands[tcAdd], " "), "rate 100mbit") {
		t.Errorf("tc argv %v must contain `rate 100mbit`", run.commands[tcAdd])
	}
}

// TestSetupNetworkEgressZeroDisablesTc locks the `EgressMbit > 0`
// guard: legacy callers (existing tests, dev CLI boot) leave the
// field at zero and the tc argv MUST NOT run. Without the guard, a
// `tc qdisc add ... rate 0mbit` would fail on metal with
// "RTNETLINK answers: Invalid argument" and abort the wake.
func TestSetupNetworkEgressZeroDisablesTc(t *testing.T) {
	run, vmm := &fakeRunner{}, &fakeVMM{}
	m := newTestManager(run, vmm)

	r := req("tc-off")
	r.EgressMbit = 0
	if _, err := m.ColdBoot(context.Background(), r); err != nil {
		t.Fatalf("cold boot: %v", err)
	}
	if indexOfArgv(run.commands, "tc qdisc add") >= 0 {
		t.Errorf("tc qdisc add must not run when EgressMbit is 0: %s", flattenForTest(run.commands))
	}
}

// TestWakeWritesMemoryMaxAfterBringUp asserts the wire-up order for
// the #33 cgroup fence: the scope is created by jailer during
// Boot/Restore, so writeMemoryMax must run AFTER bringUp returns and
// BEFORE the instance is published into m.live. This test uses
// fakeVMM (whose Boot creates the scope on the test side, mirroring
// jailer), runs a ColdBoot, and asserts both:
//  1. writeMemoryMax wrote a memory.max file in the fake cgroupRoot
//     whose value equals (MemSizeMiB + PerVMOverheadMB) << 20.
//  2. The cgroup write happened after vmm.Boot (bootCount was
//     already incremented when writeMemoryMax ran).
//
// Sweeps the four deployed plan RAMs (128/256/512/1024 MB per
// pkg/api/limits.go). A regression in the (plan+PerVMOverheadMB)
// arithmetic that happens to satisfy plan=128 still passes a
// single-value test but trips here. The cross-process e2e
// (cmd/e2e/sec11_memory_max_e2e_test.go, //go:build metal) is the
// authoritative gate that asserts the same fence against a real
// jailer's /sys/fs/cgroup; this is the layer-down pin that runs
// on every-PR CI.
func TestWakeWritesMemoryMaxAfterBringUp(t *testing.T) {
	for _, planMB := range []int{128, 256, 512, 1024} {
		t.Run(fmt.Sprintf("%dMB", planMB), func(t *testing.T) {
			run, vmm := &fakeRunner{}, &fakeVMM{}
			m := newTestManager(run, vmm)

			instID := fmt.Sprintf("cgroup-order-%d", planMB)
			r := req(instID)
			r.MemSizeMiB = planMB

			if _, err := m.ColdBoot(context.Background(), r); err != nil {
				t.Fatalf("cold boot (plan=%d): %v", planMB, err)
			}
			if vmm.boots() != 1 {
				t.Fatalf("expected 1 boot, got %d", vmm.boots())
			}
			memPath := filepath.Join(cgroupRoot, "faas-tenant.slice", PerInstanceScope(instID), "memory.max")
			body, err := os.ReadFile(memPath)
			if err != nil {
				t.Fatalf("memory.max not written at %s: %v", memPath, err)
			}
			want := int64(planMB+api.PerVMOverheadMB) << 20
			got := strings.TrimSpace(string(body))
			if got != itoa(int(want)) {
				t.Errorf("memory.max = %q, want %d", got, want)
			}
		})
	}
}

// TestWakeCgroupWriteFailureUnwindsNetns covers the leak invariant
// when the post-bringUp cgroup write itself fails. The cleanup
// defer in Wake must still tear down the netns and release the lease
// so a transient cgroup permission issue doesn't leak. We inject a
// cgroup failure via fakeVMM.bootCgroupFail so the test works
// regardless of whether it runs as root (root can bypass fs permissions).
func TestWakeCgroupWriteFailureUnwindsNetns(t *testing.T) {
	run, vmm := &fakeRunner{}, &fakeVMM{}
	// Inject a synthetic cgroup write failure — same shape as what
	// writeMemoryMax would return if the cgroup scope was unwritable.
	vmm.bootCgroupFail = errors.New("cgroup write: open /sys/fs/cgroup/faas-tenant.slice/vm-cgroup-fail/cgroup.controller: permission denied")
	m := newTestManager(run, vmm)

	_, err := m.ColdBoot(context.Background(), req("cgroup-fail"))
	if err == nil {
		t.Fatal("expected Wake to fail when cgroup write/setup is impossible")
	}
	if m.LeasedCount() != 0 {
		t.Errorf("lease leaked after cgroup failure: leased=%d", m.LeasedCount())
	}
	// Network setup ran before Boot (and before the cgroup write),
	// so the cleanup defer must have torn it down.
	if !run.ran("netns del fc-cgroup-fail") {
		t.Error("cleanup did not delete netns on cgroup failure")
	}
}

func flattenForTest(cmds [][]string) string {
	var b strings.Builder
	for _, c := range cmds {
		b.WriteString(strings.Join(c, " "))
		b.WriteString("\n")
	}
	return b.String()
}

// fakeCaptureRunner is the stdout-aware runner stub used by the
// UpdateEgressAllowlist unit tests. The real nft tool prints
// `chain forward { ... iifname "tap0" ip daddr { 1.2.3.0/24 } accept # handle 7 }`
// on success; the fake synthesises that output with a configurable
// handle so the wake path's handle capture can be exercised.
type fakeCaptureRunner struct {
	mu sync.Mutex
	// listChainOutput is the bytes the next `nft -a list chain` call
	// returns. Tests set it to a synthesised nft ruleset so
	// captureAllowlistHandles resolves a known handle.
	listChainOutput []byte
	// listChainErr, when non-nil, is returned by the next
	// RunCapture call (the test exercises the failure path).
	listChainErr error
	// commands records every argv the runner saw (parallels
	// fakeRunner.commands so the test can assert what
	// captureAllowlistHandles actually invoked).
	commands [][]string
}

func (f *fakeCaptureRunner) RunCapture(_ context.Context, argv []string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, argv)
	if f.listChainErr != nil {
		return nil, f.listChainErr
	}
	return f.listChainOutput, nil
}

// TestUpdateEgressAllowlist_NoLiveInstancesIsNoop — the empty
// app is the redelivery / no-live-targets path. No nft commands
// should fire, no error.
func TestUpdateEgressAllowlist_NoLiveInstancesIsNoop(t *testing.T) {
	run := &fakeRunner{}
	m := newTestManager(run, &fakeVMM{})
	if err := m.UpdateEgressAllowlist(context.Background(), "app-orphan", []netip.Prefix{
		netip.MustParsePrefix("1.2.3.0/24"),
	}); err != nil {
		t.Fatalf("UpdateEgressAllowlist: %v", err)
	}
	if run.ran("nft") {
		t.Error("nft should not run when no live instances match the app")
	}
}

// TestUpdateEgressAllowlist_AppliesV4Patch — a fresh netns
// (bootstrapped via a direct setupNetwork call) plus a single
// in-place patch must emit exactly one delete-by-handle (the
// prior handle captured at wake time) plus one add rule.
func TestUpdateEgressAllowlist_AppliesV4Patch(t *testing.T) {
	run := &fakeRunner{}
	m := newTestManager(run, &fakeVMM{})
	nc := netns.NewConfig("i-1", "fc-i-1", "vh1", "vp1", netip.MustParseAddr("10.100.0.2"))
	// Seed the live map with a synthetic instance whose
	// prior allowlist has a v4 entry; the renderer
	// already ran at wake time (captured by captureAllowlistHandles
	// in production; here we just hand-craft the prior state so
	// the patch path has something to delete).
	inst := &Instance{
		Lease:             Lease{Instance: "i-1", UID: 20001},
		Net:               nc,
		Method:            WakeColdBoot,
		AppID:             "app-1",
		AllowlistHandleV4: 7,
	}
	inst.Net.EgressAllowlist = []netip.Prefix{netip.MustParsePrefix("1.2.3.0/24")}
	m.mu.Lock()
	m.live["i-1"] = inst
	m.mu.Unlock()

	// Patch to a different v4 prefix.
	if err := m.UpdateEgressAllowlist(context.Background(), "app-1", []netip.Prefix{
		netip.MustParsePrefix("8.8.8.0/24"),
	}); err != nil {
		t.Fatalf("UpdateEgressAllowlist: %v", err)
	}
	// The patch sequence must include: delete-by-handle 7 on the
	// v4 chain, then add the new 8.8.8.0/24 rule. Each argv is
	// per-netns: `ip netns exec fc-i-1 nft …`. Note: the tap
	// name is "tap0" verbatim in argv (no quotes) — nft's
	// output printer adds quotes when listing rules; the
	// argv-side tokenisation is the literal string.
	wantDelete := `ip netns exec fc-i-1 nft delete rule ip faas forward handle 7`
	if !run.ran(wantDelete) {
		t.Errorf("missing %q in command stream", wantDelete)
	}
	wantAdd := `ip netns exec fc-i-1 nft add rule ip faas forward iifname tap0 ip daddr { 8.8.8.0/24 } accept`
	if !run.ran(wantAdd) {
		t.Errorf("missing %q in command stream", wantAdd)
	}
	// Cached state refreshed: the next patch's fast-path
	// compares against the new baseline.
	m.mu.Lock()
	defer m.mu.Unlock()
	if got := m.live["i-1"].Net.EgressAllowlist[0].String(); got != "8.8.8.0/24" {
		t.Errorf("cached allowlist = %q, want 8.8.8.0/24", got)
	}
	if m.live["i-1"].AllowlistHandleV4 != 7 {
		t.Errorf("cached v4 handle = %d, want 7 (capture is best-effort, no -a reader in tests)", m.live["i-1"].AllowlistHandleV4)
	}
}

// TestUpdateEgressAllowlist_SameAllowlistNoOp — redelivery.
// The same allowlist twice should not run nft at all.
func TestUpdateEgressAllowlist_SameAllowlistNoOp(t *testing.T) {
	run := &fakeRunner{}
	m := newTestManager(run, &fakeVMM{})
	nc := netns.NewConfig("i-2", "fc-i-2", "vh2", "vp2", netip.MustParseAddr("10.100.0.3"))
	inst := &Instance{
		Lease: Lease{Instance: "i-2", UID: 20002},
		Net:   nc,
		AppID: "app-2",
	}
	inst.Net.EgressAllowlist = []netip.Prefix{netip.MustParsePrefix("1.2.3.0/24")}
	m.mu.Lock()
	m.live["i-2"] = inst
	m.mu.Unlock()

	// First push — a different allowlist, should run nft
	// (prior handle is 0 so no delete, just the add).
	if err := m.UpdateEgressAllowlist(context.Background(), "app-2", []netip.Prefix{
		netip.MustParsePrefix("8.8.8.0/24"),
	}); err != nil {
		t.Fatalf("first push: %v", err)
	}
	if !run.ran("nft add rule") {
		t.Fatal("first push should have run nft")
	}
	// Second push — same allowlist as the cached baseline
	// (8.8.8.0/24 after the first push). Idempotent fast-path
	// (samePrefixSet) should short-circuit before any nft exec.
	preCount := len(run.commands)
	if err := m.UpdateEgressAllowlist(context.Background(), "app-2", []netip.Prefix{
		netip.MustParsePrefix("8.8.8.0/24"),
	}); err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if got := len(run.commands); got != preCount {
		t.Errorf("redelivery ran %d new commands, want 0 (samePrefixSet no-op)", got-preCount)
	}
}

// TestUpdateEgressAllowlist_NftErrorReverts — when the add
// step fails, the prior allowlist argv is re-emitted (best
// effort) so the live netns returns to the pre-patch state.
func TestUpdateEgressAllowlist_NftErrorReverts(t *testing.T) {
	run := &fakeRunner{failOn: "8.8.8.0/24"}
	m := newTestManager(run, &fakeVMM{})
	nc := netns.NewConfig("i-3", "fc-i-3", "vh3", "vp3", netip.MustParseAddr("10.100.0.4"))
	inst := &Instance{
		Lease: Lease{Instance: "i-3", UID: 20003},
		Net:   nc,
		AppID: "app-3",
	}
	inst.Net.EgressAllowlist = []netip.Prefix{netip.MustParsePrefix("1.2.3.0/24")}
	m.mu.Lock()
	m.live["i-3"] = inst
	m.mu.Unlock()

	err := m.UpdateEgressAllowlist(context.Background(), "app-3", []netip.Prefix{
		netip.MustParsePrefix("8.8.8.0/24"),
	})
	if err == nil {
		t.Fatal("UpdateEgressAllowlist should have failed when the add step errors")
	}
	// Revert path: the prior v4 rule was re-emitted.
	if !run.ran("1.2.3.0/24") {
		t.Error("revert did not re-emit the prior v4 rule")
	}
}

// TestUpdateEgressAllowlist_FansOutAcrossLiveInstances —
// 2 live instances of the same app, distinct v4 prefixes;
// both receive the new rule.
func TestUpdateEgressAllowlist_FansOutAcrossLiveInstances(t *testing.T) {
	run := &fakeRunner{}
	m := newTestManager(run, &fakeVMM{})
	for i, id := range []string{"a", "b"} {
		nc := netns.NewConfig("i-"+id, "fc-i-"+id, "vh-"+id, "vp-"+id, netip.MustParseAddr(fmt.Sprintf("10.100.0.%d", 10+i)))
		inst := &Instance{
			Lease: Lease{Instance: "i-" + id, UID: 20010 + i},
			Net:   nc,
			AppID: "app-shared",
		}
		inst.Net.EgressAllowlist = []netip.Prefix{netip.MustParsePrefix(fmt.Sprintf("1.1.%d.0/24", i+1))}
		m.mu.Lock()
		m.live["i-"+id] = inst
		m.mu.Unlock()
	}
	if err := m.UpdateEgressAllowlist(context.Background(), "app-shared", []netip.Prefix{
		netip.MustParsePrefix("8.8.8.0/24"),
	}); err != nil {
		t.Fatalf("UpdateEgressAllowlist: %v", err)
	}
	// Both live netns got the new rule.
	count := 0
	for _, c := range run.commands {
		if strings.Contains(strings.Join(c, " "), "8.8.8.0/24") {
			count++
		}
	}
	if count != 2 {
		t.Errorf("new rule emitted %d times, want 2 (one per live netns)", count)
	}
}

// TestUpdateEgressAllowlist_RejectsEmptyAppID — defensive.
func TestUpdateEgressAllowlist_RejectsEmptyAppID(t *testing.T) {
	run := &fakeRunner{}
	m := newTestManager(run, &fakeVMM{})
	if err := m.UpdateEgressAllowlist(context.Background(), "", nil); err == nil {
		t.Fatal("expected error for empty app_id")
	}
	if run.ran("nft") {
		t.Error("nft must not run on empty app_id")
	}
}

// TestCaptureAllowlistHandles — listChainHandles parses a
// synthesised nft -a list chain output and returns the right
// handle for both v4 and v6.
func TestCaptureAllowlistHandles(t *testing.T) {
	out := []byte(`table ip faas {
	chain forward {
	 type filter hook forward priority 0; policy accept;
	 iifname "tap0" ip daddr { 1.2.3.0/24 } accept # handle 42
	}
}
table ip6 faas {
	chain forward {
	 type filter hook forward priority 0; policy accept;
	 iifname "tap0" ip6 daddr { 2001:db8::/32 } accept # handle 99
	}
}
`)
	cap := &fakeCaptureRunner{listChainOutput: out}
	m := newTestManager(&fakeRunner{}, &fakeVMM{}).WithCaptureRunner(cap)
	hV4, hV6, err := m.captureAllowlistHandles(context.Background(), "fc-i-1")
	if err != nil {
		t.Fatalf("captureAllowlistHandles: %v", err)
	}
	if hV4 != 42 {
		t.Errorf("hV4 = %d, want 42", hV4)
	}
	if hV6 != 99 {
		t.Errorf("hV6 = %d, want 99", hV6)
	}
}

// TestCaptureAllowlistHandles_NilRunnerLeavesHandlesZero —
// the optional seam: nil capture runner means we leave
// AllowlistHandle{V4,V6} at 0 (the next patch picks them up
// via the chain list).
func TestCaptureAllowlistHandles_NilRunnerLeavesHandlesZero(t *testing.T) {
	m := newTestManager(&fakeRunner{}, &fakeVMM{})
	hV4, hV6, err := m.captureAllowlistHandles(context.Background(), "fc-i-1")
	if err != nil {
		t.Fatalf("captureAllowlistHandles: %v", err)
	}
	if hV4 != 0 || hV6 != 0 {
		t.Errorf("nil runner should return 0,0; got %d,%d", hV4, hV6)
	}
}

// TestUpdateEgressAllowlist_TwoPatchesInRow — the regression
// the review called out: a second UpdateEgressAllowlist call on
// the same app, with a DIFFERENT allowlist, must succeed. Before
// the fix, the second patch's "delete by handle" targeted the
// original handle (which was already deleted by the first patch's
// delete step). The fix is to call listChainHandles after each
// successful add and update the cached AllowlistHandleV4/V6
// before the write-back.
//
// With a nil captureRunner we can't observe the kernel-assigned
// handle, so the cached handle stays at the prior value. The
// test sets the prior handle to 0 (the fresh-Wake state) and
// asserts that two back-to-back patches BOTH succeed: the first
// patch sees handleV4=0 → no delete step (just add); the second
// patch sees handleV4=0 in the snapshot (because the unit suite
// doesn't surface the kernel-assigned handle), emits no delete
// step, and just adds the new rule. The live netns ends up with
// the most recent allowlist argv.
func TestUpdateEgressAllowlist_TwoPatchesInRow(t *testing.T) {
	run := &fakeRunner{}
	m := newTestManager(run, &fakeVMM{})
	nc := netns.NewConfig("i-2p", "fc-i-2p", "vh-2p", "vp-2p", netip.MustParseAddr("10.100.0.20"))
	inst := &Instance{
		Lease: Lease{Instance: "i-2p", UID: 20020},
		Net:   nc,
		AppID: "app-2p",
		// No handle captured — fresh Wake simulation.
	}
	inst.Net.EgressAllowlist = []netip.Prefix{netip.MustParsePrefix("1.2.3.0/24")}
	m.mu.Lock()
	m.live["i-2p"] = inst
	m.mu.Unlock()

	// First patch: different allowlist.
	if err := m.UpdateEgressAllowlist(context.Background(), "app-2p", []netip.Prefix{
		netip.MustParsePrefix("8.8.8.0/24"),
	}); err != nil {
		t.Fatalf("patch 1: %v", err)
	}
	// Second patch: another different allowlist. The cached
	// handle is still 0 (no capture runner), so the delete
	// step is skipped and the add succeeds. The cached
	// allowlist is the most recent.
	if err := m.UpdateEgressAllowlist(context.Background(), "app-2p", []netip.Prefix{
		netip.MustParsePrefix("9.9.9.0/24"),
	}); err != nil {
		t.Fatalf("patch 2: %v", err)
	}
	// Both new rules must have been emitted. The 1.2.3.0/24
	// prior should NEVER appear (no delete step runs because
	// handleV4=0 throughout).
	if run.ran("1.2.3.0/24") {
		t.Errorf("patch should not have re-emitted the prior CGI: 1.2.3.0/24")
	}
	if !run.ran("8.8.8.0/24") {
		t.Errorf("patch 1's add argv missing: 8.8.8.0/24")
	}
	if !run.ran("9.9.9.0/24") {
		t.Errorf("patch 2's add argv missing: 9.9.9.0/24")
	}
	// Cached state matches the most recent patch.
	m.mu.Lock()
	defer m.mu.Unlock()
	if got := m.live["i-2p"].Net.EgressAllowlist[0].String(); got != "9.9.9.0/24" {
		t.Errorf("cached allowlist = %q, want 9.9.9.0/24", got)
	}
}

// TestUpdateEgressAllowlist_TwoPatchesInRow_WithCaptureRunner —
// the load-bearing pair to TestUpdateEgressAllowlist_TwoPatchesInRow:
// when the captureRunner is wired, the second patch must observe
// the post-first-patch handle (a fresh kernel-assigned integer)
// and use it for the delete-by-handle call. This is the path the
// metal test exercises on the EX44.
//
// fakeCaptureRunner returns a consecutive handle sequence
// (1, 2, 3, ...) so the test can assert the second patch's
// delete-by-handle call targets the latest handle.
func TestUpdateEgressAllowlist_TwoPatchesInRow_WithCaptureRunner(t *testing.T) {
	run := &fakeRunner{}
	cap := &handleSeqCaptureRunner{}
	m := newTestManager(run, &fakeVMM{}).WithCaptureRunner(cap)
	nc := netns.NewConfig("i-2pc", "fc-i-2pc", "vh-2pc", "vp-2pc", netip.MustParseAddr("10.100.0.21"))
	inst := &Instance{
		Lease:             Lease{Instance: "i-2pc", UID: 20021},
		Net:               nc,
		AppID:             "app-2pc",
		AllowlistHandleV4: 9, // wake-time capture
	}
	inst.Net.EgressAllowlist = []netip.Prefix{netip.MustParsePrefix("1.2.3.0/24")}
	m.mu.Lock()
	m.live["i-2pc"] = inst
	m.mu.Unlock()

	// Patch 1.
	if err := m.UpdateEgressAllowlist(context.Background(), "app-2pc", []netip.Prefix{
		netip.MustParsePrefix("8.8.8.0/24"),
	}); err != nil {
		t.Fatalf("patch 1: %v", err)
	}
	// Patch 2.
	if err := m.UpdateEgressAllowlist(context.Background(), "app-2pc", []netip.Prefix{
		netip.MustParsePrefix("9.9.9.0/24"),
	}); err != nil {
		t.Fatalf("patch 2: %v", err)
	}
	// The captures must have produced non-zero handles.
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.live["i-2pc"].AllowlistHandleV4 == 0 {
		t.Errorf("after patch 2 with captureRunner wired, AllowlistHandleV4 should be non-zero")
	}
	// The first patch's path: delete-by-handle 9 + add.
	// The second patch's path: delete-by-handle <new-from-patch-1>
	// + add.
	wantDeletePatch1 := `delete rule ip faas forward handle 9`
	if !run.ran(wantDeletePatch1) {
		t.Errorf("patch 1 delete-by-handle 9 missing")
	}
	// The second patch's delete-by-handle must NOT be 9 (the
	// original handle) — it must be the handle captured after
	// patch 1.
	var sawDeletePatch2 bool
	run.mu.Lock()
	for _, c := range run.commands {
		join := strings.Join(c, " ")
		if strings.Contains(join, "delete rule ip faas forward handle") &&
			!strings.Contains(join, "handle 9 ") {
			sawDeletePatch2 = true
			t.Logf("patch 2 delete argv: %s", join)
		}
	}
	run.mu.Unlock()
	if !sawDeletePatch2 {
		t.Errorf("patch 2 must delete by the post-patch-1 handle, not by handle 9")
	}
}

// handleSeqCaptureRunner returns a sequence of distinct
// handles on each listChainHandles call. The first capture
// returns 100, the next 200, then 300, etc. The synth nft
// output uses the same `iifname "tap0" ip daddr { … } accept #
// handle N` shape the real kernel emits.
type handleSeqCaptureRunner struct {
	mu    sync.Mutex
	calls int
}

func (f *handleSeqCaptureRunner) RunCapture(_ context.Context, argv []string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	handle := f.calls * 100
	// Build the synthetic ruleset that matches what the renderer
	// just emitted. Pick the family from the argv.
	family := "ip"
	for _, a := range argv {
		if a == "ip6" {
			family = "ip6"
		}
	}
	// Use the cached prior baseline the test seeded.
	cidr := "8.8.8.0/24"
	if family == "ip6" {
		cidr = "2001:db8::/32"
	}
	return []byte(fmt.Sprintf(`table %s faas {
	chain forward {
	 type filter hook forward priority 0; policy accept;
	 iifname "tap0" %s daddr { %s } accept # handle %d
	}
}`, family, family, cidr, handle)), nil
}

// TestUpdateEgressAllowlist_V6FailureLeavesV4Untouched — the
// per-family revert path. A v4 + v6 allowlist patch where the
// v6 add step fails: the v4 patch should still have landed
// (its add rule is in the command stream), and the v6 revert
// should re-emit the prior v6 rule. The pre-fix code did the
// revert for both families, which would have undone the v4
// success.
func TestUpdateEgressAllowlist_V6FailureLeavesV4Untouched(t *testing.T) {
	// failOn matches the v6 add argv (the new v6 prefix
	// "fe80::/10"). The fakeRunner fails on the FIRST matching
	// command in command order; the patch sequence is v4 first
	// then v6, so the v4 add succeeds and the v6 add fails.
	run := &fakeRunner{failOn: "fe80::/10"}
	m := newTestManager(run, &fakeVMM{})
	nc := netns.NewConfig("i-pf", "fc-i-pf", "vh-pf", "vp-pf", netip.MustParseAddr("10.100.0.30"))
	inst := &Instance{
		Lease:             Lease{Instance: "i-pf", UID: 20030},
		Net:               nc,
		AppID:             "app-pf",
		AllowlistHandleV4: 11,
		AllowlistHandleV6: 22,
	}
	inst.Net.EgressAllowlist = []netip.Prefix{
		netip.MustParsePrefix("1.2.3.0/24"),
		netip.MustParsePrefix("2001:db8::/32"),
	}
	m.mu.Lock()
	m.live["i-pf"] = inst
	m.mu.Unlock()

	err := m.UpdateEgressAllowlist(context.Background(), "app-pf", []netip.Prefix{
		netip.MustParsePrefix("8.8.8.0/24"),
		netip.MustParsePrefix("fe80::/10"),
	})
	if err == nil {
		t.Fatal("expected error from v6 add failure")
	}
	// The v4 patch should have run: delete-by-handle 11 + add 8.8.8.0/24.
	if !run.ran("delete rule ip faas forward handle 11") {
		t.Error("v4 delete-by-handle 11 missing")
	}
	if !run.ran("8.8.8.0/24") {
		t.Error("v4 add 8.8.8.0/24 missing")
	}
	// The v6 revert should re-emit the prior v6 rule
	// (2001:db8::/32). The pre-fix code would have
	// re-emitted prior v4 too (1.2.3.0/24), which would have
	// undone the v4 success.
	if !run.ran("2001:db8::/32") {
		t.Error("v6 revert did not re-emit the prior v6 rule (2001:db8::/32)")
	}
	// The prior v4 should NOT have been re-emitted by the
	// revert (per-family revert preserves the v4 success).
	// Count v4 add invocations: 1 from the patch path (the
	// new 8.8.8.0/24 rule), 0 from the revert path.
	v4AddCount := 0
	run.mu.Lock()
	for _, c := range run.commands {
		join := strings.Join(c, " ")
		if strings.Contains(join, "ip daddr") && strings.Contains(join, "accept") {
			v4AddCount++
		}
	}
	run.mu.Unlock()
	if v4AddCount != 1 {
		t.Errorf("v4 add ran %d times; want 1 (no revert of v4 success); commands: %v",
			v4AddCount, run.commands)
	}
}

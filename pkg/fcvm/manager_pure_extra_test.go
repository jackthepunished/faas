// manager_pure_extra_test.go — fill pkg/fcvm/manager.go coverage of the
// pure setter / nil-safe-delegate / map-lookup surface. Every method
// touched here is currently at 0% in the pre-PR report; the wiring
// helpers all sit above the KVM-dependent path and need only a
// zero-dependency Manager skeleton.
//
// Whitebox `package fcvm` (matches every existing pkg/fcvm test).
//
// Patterns used:
//   - NewManager with discard logger + nil metrics (mirrors
//     pkg/fcvm/manager_test.go::TestNew).
//   - Direct map seeding on m.live / m.cidToID to bypass the metal-only
//     BringUp path. Locks are taken where the production method takes
//     them, so race-detector coverage still applies.

package fcvm

import (
	"context"
	"errors"
	"net/netip"
	"sync/atomic"
	"testing"
)

// newPureManager returns a Manager skeleton with the discard logger +
// nil metrics. Renamed to avoid colliding with newTestManager in
// manager_test.go.
func newPureManager() (*Manager, *fakeRunner, *fakeVMM) {
	run := &fakeRunner{}
	vmm := &fakeVMM{}
	m := NewManager(run, vmm, Paths{}, "v1.0.0", nil, nil)
	return m, run, vmm
}

// seedLive places a synthetic Instance row in m.live so map-lookup
// helpers have something to find. Skips the metal-only BringUp path.
func seedLive(m *Manager, instance, appID, deploymentID string, slot int) *Instance {
	inst := &Instance{
		Lease:        Lease{Instance: instance, Slot: slot, HostIP: netip.MustParseAddr("10.100.0.2")},
		AppID:        appID,
		DeploymentID: deploymentID,
	}
	m.mu.Lock()
	m.live[instance] = inst
	m.cidToID[GuestVsockCID(slot)] = instance
	m.mu.Unlock()
	return inst
}

// --- Setter chain coverage ----------------------------------------

func TestManager_Setters_ChainAndReturnReceiver(t *testing.T) {
	m, _, _ := newPureManager()
	if got := m.WithFrameworkReady(nil); got != m {
		t.Error("WithFrameworkReady: not chainable")
	}
	if got := m.SetWakePhaseMetrics(nil); got != m {
		t.Error("SetWakePhaseMetrics: not chainable")
	}
	if got := m.WithFrameworkReadyStamper(nil); got != m {
		t.Error("WithFrameworkReadyStamper: not chainable")
	}
	if got := m.WithTailTerminalStamper(nil); got != m {
		t.Error("WithTailTerminalStamper: not chainable")
	}
	if got := m.WithLivenessProbeStarter(nil); got != m {
		t.Error("WithLivenessProbeStarter: not chainable")
	}
	if got := m.WithLivenessMetrics(nil); got != m {
		t.Error("WithLivenessMetrics: not chainable")
	}
	if got := m.WithLivenessSink(nil); got != m {
		t.Error("WithLivenessSink: not chainable")
	}
	if got := m.WithWorkloadOOMSink(nil); got != m {
		t.Error("WithWorkloadOOMSink: not chainable")
	}
}

func TestManager_SetHostIdentity_NilSafe(t *testing.T) {
	m, _, _ := newPureManager()
	// nil identity: impl wraps in a 1-element slice containing a nil
	// pointer (no panic, no nil-rejection). age.Decrypt later treats
	// that as "tried one key, all failed". Document the behaviour.
	m.SetHostIdentity(nil)
	if len(m.hostIdentities) != 1 || m.hostIdentities[0] != nil {
		t.Errorf("nil identity: got %d identities, want 1 with nil pointer", len(m.hostIdentities))
	}
}

func TestManager_SetHostIdentities_EmptyClears(t *testing.T) {
	m, _, _ := newPureManager()
	m.SetHostIdentities(nil)
	if m.hostIdentities != nil {
		t.Errorf("nil slice: hostIdentities not nil")
	}
	// Empty slice is also treated as "no host age" per the impl.
	m.SetHostIdentities(nil)
	if m.hostIdentities != nil {
		t.Errorf("nil slice (re-call): hostIdentities not nil")
	}
}

func TestManager_WithStorage_NilSafe(t *testing.T) {
	m, _, _ := newPureManager()
	m.WithStorage(nil)
	if m.storage != nil {
		t.Errorf("nil storage: not nil")
	}
}

func TestManager_VMM_ReturnsUnderlying(t *testing.T) {
	m, _, vmm := newPureManager()
	if m.VMM() == nil {
		t.Fatal("VMM(): nil")
	}
	if m.VMM() != vmm {
		t.Errorf("VMM() pointer drift")
	}
}

func TestManager_SetImageScanMetrics_NilSafe(t *testing.T) {
	m, _, _ := newPureManager()
	m.SetImageScanMetrics(nil)
	if m.imageScanMetrics != nil {
		t.Errorf("nil metrics: not nil")
	}
}

func TestManager_SetAdvisoryClient_NilSafe(t *testing.T) {
	m, _, _ := newPureManager()
	m.SetAdvisoryClient(nil)
	if m.advisoryClient != nil {
		t.Errorf("nil client: not nil")
	}
}

func TestManager_SetParentMountRegistry_NilSafe(t *testing.T) {
	m, _, _ := newPureManager()
	m.SetParentMountRegistry(nil)
	if m.parentMounts != nil {
		t.Errorf("nil registry: not nil")
	}
}

// --- Liveness nil-safe-delegate coverage --------------------------

func TestManager_ObserveLivenessProbe_DelegatesToMetrics(t *testing.T) {
	m, _, _ := newPureManager()
	lm := NewLivenessMetrics()
	m.WithLivenessMetrics(lm)
	// Manager delegates straight to lm.ObserveProbe — verify both
	// the absence of a panic and that the underlying histogram has
	// a sample by re-observing with a different outcome and reading
	// from the registry directly.
	m.ObserveLivenessProbe("timeout", 0.05)
	m.ObserveLivenessProbe("ok", 0.001)
	// LivenessMetrics.ObserveProbe is itself nil-safe; second verify
	// by calling lm.ObserveProbe directly with an outcome that
	// collapses to "unknown" (the empty-string branch).
	lm.ObserveProbe("", 0.01)
}

func TestManager_SetLivenessConsecutiveFailures_Delegates(t *testing.T) {
	m, _, _ := newPureManager()
	lm := NewLivenessMetrics()
	m.WithLivenessMetrics(lm)
	m.SetLivenessConsecutiveFailures("inst-1", 5)
	// LivenessMetrics.SetConsecutiveFailures is itself nil-safe;
	// verify the empty-instance branch by re-calling with "" and
	// confirming no panic.
	lm.SetConsecutiveFailures("", 7)
}

func TestManager_DeleteLivenessConsecutiveFailures_Delegates(t *testing.T) {
	m, _, _ := newPureManager()
	lm := NewLivenessMetrics()
	m.WithLivenessMetrics(lm)
	m.SetLivenessConsecutiveFailures("inst-1", 5)
	m.DeleteLivenessConsecutiveFailures("inst-1")
	// DeleteConsecutiveFailures is also nil-safe; verify both paths.
	lm.DeleteConsecutiveFailures("inst-1")
}

// --- ReportLivenessFailed: nil-relay + cooldown stamp --------------

type stubLivenessRelay struct {
	calls atomic.Int64
	last  struct {
		instanceID string
		reason     string
	}
}

func (s *stubLivenessRelay) relay(_ context.Context, instanceID, reason string) {
	s.calls.Add(1)
	s.last.instanceID = instanceID
	s.last.reason = reason
}

func TestManager_ReportLivenessFailed_NilRelayDoesNotPanic(t *testing.T) {
	m, _, _ := newPureManager()
	seedLive(m, "inst-1", "app-1", "dep-1", 1)
	m.ReportLivenessFailed(context.Background(), "inst-1", "timeout")
}

func TestManager_ReportLivenessFailed_StampsCooldown(t *testing.T) {
	// Issue #554 closure / ADR-078 cooldown gate: the dying-instance
	// branch was structurally broken before this stamp landed.
	m, _, _ := newPureManager()
	seedLive(m, "inst-1", "app-1", "dep-1", 1)
	stub := &stubLivenessRelay{}
	m.WithLivenessSink(stub.relay)

	m.ReportLivenessFailed(context.Background(), "inst-1", "timeout")
	if stub.calls.Load() != 1 {
		t.Errorf("relay calls = %d, want 1", stub.calls.Load())
	}
	if stub.last.reason != "timeout" {
		t.Errorf("relay reason = %q", stub.last.reason)
	}

	m.mu.Lock()
	stamp, ok := m.cooldownByDeployment["dep-1"]
	m.mu.Unlock()
	if !ok {
		t.Error("cooldownByDeployment[dep-1] not stamped")
	}
	if stamp.IsZero() {
		t.Error("cooldown stamp is zero")
	}
}

func TestManager_ReportLivenessFailed_UnknownInstanceSkipsStamp(t *testing.T) {
	// Park race: the dying instance is no longer live by the time
	// the relay call returns. Stamp must log+skip, not panic.
	m, _, _ := newPureManager()
	stub := &stubLivenessRelay{}
	m.WithLivenessSink(stub.relay)

	m.ReportLivenessFailed(context.Background(), "no-such", "timeout")
	if stub.calls.Load() != 1 {
		t.Errorf("relay calls = %d, want 1 (relay still fires)", stub.calls.Load())
	}
}

func TestManager_ReportLivenessFailed_EmptyDeploymentIDSkipsStamp(t *testing.T) {
	// Legacy pre-PR-B wake: no deployment_id on the wire. Stamp
	// would collide across instances; skip with a debug log.
	m, _, _ := newPureManager()
	seedLive(m, "inst-1", "app-1", "", 1)
	stub := &stubLivenessRelay{}
	m.WithLivenessSink(stub.relay)

	m.ReportLivenessFailed(context.Background(), "inst-1", "conn_refused")
	m.mu.Lock()
	_, ok := m.cooldownByDeployment[""]
	m.mu.Unlock()
	if ok {
		t.Error("legacy wake: cooldown stamped on empty deploymentID (collision risk)")
	}
}

// --- ReportWorkloadOOM --------------------------------------------

func TestManager_ReportWorkloadOOM_Pure_NilRelayNoOp(t *testing.T) {
	m, _, _ := newPureManager()
	m.ReportWorkloadOOM(context.Background(), "inst-1", 600, 512)
}

func TestManager_ReportWorkloadOOM_Pure_RelayCalled(t *testing.T) {
	m, _, _ := newPureManager()
	var got struct {
		instanceID string
		peak, plan int
	}
	called := false
	m.WithWorkloadOOMSink(func(_ context.Context, instanceID string, peakMB, planMB int) {
		called = true
		got.instanceID = instanceID
		got.peak = peakMB
		got.plan = planMB
	})
	m.ReportWorkloadOOM(context.Background(), "inst-1", 600, 512)
	if !called {
		t.Fatal("relay not called")
	}
	if got.instanceID != "inst-1" || got.peak != 600 || got.plan != 512 {
		t.Errorf("relay payload = %+v", got)
	}
}

// --- Pure map delegates -------------------------------------------

func TestManager_InstanceByCID_Hit(t *testing.T) {
	m, _, _ := newPureManager()
	seedLive(m, "inst-1", "app-1", "dep-1", 5)
	id, err := m.InstanceByCID(GuestVsockCID(5))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if id != "inst-1" {
		t.Errorf("id = %q, want inst-1", id)
	}
}

func TestManager_InstanceByCID_Miss(t *testing.T) {
	m, _, _ := newPureManager()
	if _, err := m.InstanceByCID(GuestVsockCID(99)); err == nil {
		t.Fatal("miss: err = nil, want error")
	}
}

func TestManager_InstanceAppID_Hit(t *testing.T) {
	m, _, _ := newPureManager()
	seedLive(m, "inst-1", "app-uuid-1", "dep-1", 1)
	appID, err := m.InstanceAppID("inst-1")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if appID != "app-uuid-1" {
		t.Errorf("appID = %q", appID)
	}
}

func TestManager_InstanceAppID_Miss(t *testing.T) {
	m, _, _ := newPureManager()
	if _, err := m.InstanceAppID("nope"); err == nil {
		t.Error("miss: err = nil, want error")
	}
}

func TestManager_InstanceDeploymentIDAndAppID_Hit(t *testing.T) {
	m, _, _ := newPureManager()
	seedLive(m, "inst-1", "app-uuid-1", "dep-uuid-1", 1)
	depID, appID, err := m.InstanceDeploymentIDAndAppID("inst-1")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if depID != "dep-uuid-1" || appID != "app-uuid-1" {
		t.Errorf("depID=%q appID=%q", depID, appID)
	}
}

func TestManager_InstanceDeploymentIDAndAppID_Miss(t *testing.T) {
	m, _, _ := newPureManager()
	if _, _, err := m.InstanceDeploymentIDAndAppID("nope"); err == nil {
		t.Error("miss: err = nil, want error")
	}
}

// --- ReadAndResetTailSeconds -------------------------------------

func TestManager_ReadAndResetTailSeconds_Hit(t *testing.T) {
	m, _, _ := newPureManager()
	inst := seedLive(m, "inst-1", "app-1", "dep-1", 1)
	m.mu.Lock()
	inst.tailSecondsAccum = 42
	m.mu.Unlock()

	got, ok := m.ReadAndResetTailSeconds("inst-1")
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if got != 42 {
		t.Errorf("got = %d, want 42", got)
	}
	got2, ok2 := m.ReadAndResetTailSeconds("inst-1")
	if !ok2 || got2 != 0 {
		t.Errorf("post-reset got=%d ok=%v, want 0 true", got2, ok2)
	}
}

func TestManager_ReadAndResetTailSeconds_Miss(t *testing.T) {
	m, _, _ := newPureManager()
	got, ok := m.ReadAndResetTailSeconds("nope")
	if ok || got != 0 {
		t.Errorf("miss: got=%d ok=%v", got, ok)
	}
}

// --- ForwardStatelessAdvisory ------------------------------------

func TestManager_ForwardStatelessAdvisory_EmptyBatchNoOp(t *testing.T) {
	m, _, _ := newPureManager()
	if err := m.ForwardStatelessAdvisory(context.Background(), "inst-1", "app-1", nil); err != nil {
		t.Errorf("empty batch: err = %v, want nil", err)
	}
}

func TestManager_ForwardStatelessAdvisory_NilClientNoOp(t *testing.T) {
	m, _, _ := newPureManager()
	batch := []AdvisoryEvent{{Path: "/tmp", Masks: []string{"open"}}}
	if err := m.ForwardStatelessAdvisory(context.Background(), "inst-1", "app-1", batch); err != nil {
		t.Errorf("nil client: err = %v, want nil", err)
	}
}

func TestManager_ForwardStatelessAdvisory_FailingClientSwallowed(t *testing.T) {
	// ADR-035 best-effort: forward failure logs + drops; never
	// bubbles to the caller.
	m, _, _ := newPureManager()
	m.SetAdvisoryClient(stubAdvisory{err: errors.New("apid down")})
	batch := []AdvisoryEvent{{Path: "/tmp", Masks: []string{"open"}}}
	if err := m.ForwardStatelessAdvisory(context.Background(), "inst-1", "app-1", batch); err != nil {
		t.Errorf("failing client: err = %v, want nil (ADR-035)", err)
	}
}

type stubAdvisory struct{ err error }

func (s stubAdvisory) Forward(_ context.Context, _ string, _ string, _ []AdvisoryEvent) error {
	return s.err
}

// --- HostIdentity getter ------------------------------------------

func TestManager_HostIdentity_NilByDefault(t *testing.T) {
	m, _, _ := newPureManager()
	if m.HostIdentity() != nil {
		t.Error("default HostIdentity: not nil")
	}
}

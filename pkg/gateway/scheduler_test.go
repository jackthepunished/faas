package gateway

import (
	"context"
	"errors"
	"testing"

	scheddpb "github.com/onebox-faas/faas/api/proto/onebox/faas/schedd/v1"
)

func TestFakeSchedulerAdmitInstance(t *testing.T) {
	s := NewFakeScheduler("node-fake-1").WithInstanceID("i-7").WithWakeID("w-9")
	instanceID, nodeID, wakeID, method, atCap, err := s.AdmitInstance(context.Background(), "app-1")
	if err != nil {
		t.Fatalf("AdmitInstance err = %v", err)
	}
	if atCap {
		t.Errorf("atCapacity = true; want false on admit path")
	}
	if nodeID != "node-fake-1" {
		t.Errorf("nodeID = %q, want node-fake-1", nodeID)
	}
	if instanceID != "i-7" {
		t.Errorf("instanceID = %q, want i-7", instanceID)
	}
	if wakeID != "w-9" {
		t.Errorf("wakeID = %q, want w-9", wakeID)
	}
	// Default FakeScheduler method is WakeMethodColdBoot → raw int32 = 0.
	if method != 0 {
		t.Errorf("method = %d, want 0 (cold boot default)", method)
	}
	if got := s.Calls(); got != 1 {
		t.Errorf("Calls = %d, want 1", got)
	}
	if got := s.AdmitsFor("app-1"); got != 1 {
		t.Errorf("AdmitsFor = %d, want 1", got)
	}
}

func TestFakeSchedulerMintsFreshInstanceID(t *testing.T) {
	s := NewFakeScheduler("node-fake-1")
	ids := map[string]bool{}
	for i := 0; i < 3; i++ {
		id, _, _, _, _, err := s.AdmitInstance(context.Background(), "app-1")
		if err != nil {
			t.Fatalf("AdmitInstance: %v", err)
		}
		if ids[id] {
			t.Errorf("duplicate instance id %q on call #%d", id, i)
		}
		ids[id] = true
	}
}

func TestFakeSchedulerWithErr(t *testing.T) {
	want := errors.New("boom")
	s := NewFakeScheduler("node-fake-1").WithErr(want)
	_, _, _, _, _, err := s.AdmitInstance(context.Background(), "app-1")
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
}

func TestNoopSchedulerReturnsUnconfigured(t *testing.T) {
	_, _, _, _, _, err := NoopScheduler{}.AdmitInstance(context.Background(), "app-1")
	if !errors.Is(err, ErrSchedulerUnconfigured) {
		t.Errorf("err = %v, want ErrSchedulerUnconfigured", err)
	}
}

// TestWireWakeMethod pins the gateway-side wire constants to the
// scheddpb.WakeMethod enum values declared in
// api/proto/onebox/faas/schedd/v1/schedd.proto:132-135. The proto
// file's "Wire-stable: do NOT reorder; new slots go at the end" rule
// means a future proto reordering would silently regress the
// wake-locality classifier without tripping any other test. This
// guard is the single source of truth: if the proto enum values
// shift, this test fails loud.
//
// Tests live in this package (not pkg/scheddgrpc) because the
// constants are owned by pkg/gateway. The test only imports the
// proto package here — runtime code in pkg/gateway stays free of the
// protobuf dependency.
func TestWireWakeMethod(t *testing.T) {
	if WireWakeColdBoot != int32(scheddpb.WakeMethod_WAKE_COLD_BOOT) {
		t.Errorf("WireWakeColdBoot = %d, want %d (scheddpb.WAKE_COLD_BOOT)",
			WireWakeColdBoot, scheddpb.WakeMethod_WAKE_COLD_BOOT)
	}
	if WireWakeRestore != int32(scheddpb.WakeMethod_WAKE_RESTORE) {
		t.Errorf("WireWakeRestore = %d, want %d (scheddpb.WAKE_RESTORE)",
			WireWakeRestore, scheddpb.WakeMethod_WAKE_RESTORE)
	}
}

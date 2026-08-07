package fcvm

import "testing"

// TestCoverageSliceManagerSetters drives the 0%-coverage With* setters
// on Manager. Most setters are pure delegation; the test exercises
// each setter and asserts the manager pointer is returned (chainable).
func TestCoverageSliceManagerSetters(t *testing.T) {
	run := &fakeRunner{}
	vmm := &fakeVMM{}
	m := newTestManager(run, vmm)

	// Set* / With* setters that return the manager (chainable).
	if got := m.WithFrameworkReadyStamper(nil); got != m {
		t.Errorf("WithFrameworkReadyStamper returned different pointer")
	}
	if got := m.WithLivenessProbeStarter(nil); got != m {
		t.Errorf("WithLivenessProbeStarter returned different pointer")
	}
	if got := m.WithLivenessMetrics(nil); got != m {
		t.Errorf("WithLivenessMetrics returned different pointer")
	}
	if got := m.WithLivenessSink(nil); got != m {
		t.Errorf("WithLivenessSink returned different pointer")
	}

	// Set* setters that return nothing.
	m.SetHostIdentities(nil)
	m.SetImageScanMetrics(nil)
	m.SetParentMountRegistry(nil)
}

// TestCoverageSliceLivenessRegistry drives the liveness registry
// constructor. The probe-loop start/cancel seams require a running
// VM, so we don't exercise them here.
func TestCoverageSliceLivenessRegistry(t *testing.T) {
	r := NewLivenessRegistry()
	if r == nil {
		t.Fatal("NewLivenessRegistry returned nil")
	}
}

// TestCoverageSliceManagerGetters exercises the Manager getters that
// are at 0% in the coverage profile.
func TestCoverageSliceManagerGetters(t *testing.T) {
	run := &fakeRunner{}
	vmm := &fakeVMM{}
	m := newTestManager(run, vmm)

	// VMM() returns the embedded vmm; assert non-nil.
	if m.VMM() == nil {
		t.Error("VMM() returned nil")
	}
}

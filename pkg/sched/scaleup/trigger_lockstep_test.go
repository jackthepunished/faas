// PR-A lockstep test (scaleup). See pkg/sched/floor/trigger_lockstep_test.go
// for the package-architecture rationale (floor_test is the external test
// package; floor → sched cycle is test-time only and bypasses the
// production import graph).
package scaleup_test

import (
	"testing"

	"github.com/onebox-faas/faas/pkg/sched"
	"github.com/onebox-faas/faas/pkg/sched/scaleup"
)

func TestWakeBootTrigger_Lockstep(t *testing.T) {
	if got, want := scaleup.WakeBootTriggerScaleup, sched.TriggerScaleup; got != want {
		t.Errorf("scaleup drift: got %q want %q (pkg/sched/scaleup/trigger.go:20 mirror must equal sched.TriggerScaleup; see ADR-123)",
			got, want)
	}
}

// PR-A lockstep test (targets). See pkg/sched/floor/trigger_lockstep_test.go
// for the package-architecture rationale.
package targets_test

import (
	"testing"

	"github.com/onebox-faas/faas/pkg/sched"
	"github.com/onebox-faas/faas/pkg/sched/targets"
)

func TestWakeBootTrigger_Lockstep(t *testing.T) {
	if got, want := targets.WakeBootTriggerTargets, sched.TriggerTargets; got != want {
		t.Errorf("targets drift: got %q want %q (pkg/sched/targets/trigger.go:18 mirror must equal sched.TriggerTargets; see ADR-123)",
			got, want)
	}
}

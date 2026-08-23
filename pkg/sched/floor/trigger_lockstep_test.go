// PR-A lockstep test (floor) — pins wakeBootTriggerFloor / FloorDep
// against pkg/sched/triggers.go's canonical enum. Closes PR #1015
// review finding #4.
//
// Lives in package floor_test so the external test can import both
// `floor` (to read WakeBootTriggerFloor from export_test.go) and
// `pkg/sched` (to read sched.TriggerFloor). The import graph is:
//
//	floor_test → floor        (test-time)
//	floor_test → pkg/sched    (test-time)
//	pkg/sched  → floor        (production: loop.go)
//
// No cycle: `floor` doesn't import `pkg/sched/floor_test` (production
// code never imports test packages) and `pkg/sched` doesn't import
// back into the test binary. Goes through fine.
// `pkg/sched/floor/trigger.go:16-24` documents the local-mirror
// rationale (avoiding the floor → sched production-code cycle).
package floor_test

import (
	"testing"

	"github.com/onebox-faas/faas/pkg/sched"
	"github.com/onebox-faas/faas/pkg/sched/floor"
)

func TestWakeBootTrigger_Lockstep(t *testing.T) {
	cases := []struct {
		name    string
		got     string
		canon   string // pkg/sched/triggers.go canonical constant
		canonNm string // human name for the error message
	}{
		{
			name:    "floor per-app",
			got:     floor.WakeBootTriggerFloor,
			canon:   sched.TriggerFloor,
			canonNm: "sched.TriggerFloor",
		},
		{
			name:    "floor per-deployment",
			got:     floor.WakeBootTriggerFloorDep,
			canon:   sched.TriggerFloorDep,
			canonNm: "sched.TriggerFloorDep",
		},
	}
	for _, tc := range cases {
		if tc.got != tc.canon {
			t.Errorf("%s drift: got %q want %q (pkg/sched/floor/trigger.go mirror must equal %s; see ADR-123 — drift here makes the dashboard render 'Unknown' for floor-driven wakes)",
				tc.name, tc.got, tc.canon, tc.canonNm)
		}
	}
}

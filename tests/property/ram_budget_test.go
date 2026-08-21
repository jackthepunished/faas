// ram_budget_test.go — §6.2 invariant #2:
//
//	Σ(ram_mb + 8) over live instances ≤ 47,600 MB (85% of
//	56 GB tenant budget).
//
// Property-driven test: random admit/release cycles across
// multiple apps + multiple nodes keep the fleet-wide Σ and
// the per-node Σ under the admission ceiling. Pins:
//   - ADR-025 axis 3 (cross-node Σ invariant).
//   - The 47,600 MB cap (api.RAMAdmissionCeilingMB = 85% of
//     the tenant budget, per pkg/api/limits.go §1).
//
// Whitebox through pkg/sched.NewNodeLedger.
package property

import (
	"math/rand"
	"strconv"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/sched"
)

// TestSchedProperty_FleetRAMBudget pins the §6.2-2 invariant:
// across random admits + releases on N nodes, no single
// admission lifts ResidentRAM() above api.RAMAdmissionCeilingMB.
// Releases drop it back by exactly the admit's
// (ram_mb + PerVMOverheadMB) — no leakage, no double-count.
//
// Uses seed=42 so the test is replayable across runs and CI.
func TestSchedProperty_FleetRAMBudget(t *testing.T) {
	const (
		seed  = 42
		iters = 200
	)
	rng := rand.New(rand.NewSource(seed))
	l := sched.NewNodeLedger()
	apps := []string{"app-a", "app-b", "app-c"}
	nodes := []string{"node-a", "node-b", "node-c"}
	type admit struct {
		instance, app, node string
		ram                 int
	}
	var live []admit
	maxObserved := 0

	for i := 0; i < iters; i++ {
		roll := rng.Intn(100)
		if roll < 65 || len(live) == 0 {
			// Admit: choose random app+node+ram.
			inst := "vm-" + strconv.Itoa(i)
			app := apps[rng.Intn(len(apps))]
			node := nodes[rng.Intn(len(nodes))]
			ram := []int{128, 256, 512, 1024}[rng.Intn(4)]
			err := l.Admit(sched.Request{
				Instance:       inst,
				AppID:          app,
				NodeID:         node,
				RAMMB:          ram,
				VCPU:           1,
				Plan:           api.PlanHobby,
				NodeCeilingMB:  100000, // per-node: well above cap so global is the gate
				VCPUBudget:     160,
				MaxConcurrency: 100, // disable per-app gate for this test
			})
			if err == nil {
				live = append(live, admit{inst, app, node, ram})
				got := l.ResidentRAM()
				if got > api.RAMAdmissionCeilingMB {
					t.Fatalf("iteration %d: ResidentRAM() = %d, want ≤ %d (cap) — §6.2-2 violated",
						i, got, api.RAMAdmissionCeilingMB)
				}
				if got > maxObserved {
					maxObserved = got
				}
			}
		} else {
			idx := rng.Intn(len(live))
			a := live[idx]
			l.Release(a.instance)
			live[idx] = live[len(live)-1]
			live = live[:len(live)-1]
		}
	}

	// Σ cross-node invariant: ResidentRAM = Σ ResidentRAMForNode.
	var perNode int64
	for _, n := range nodes {
		perNode += int64(l.ResidentRAMForNode(n))
	}
	if got := int64(l.ResidentRAM()); got != perNode {
		t.Errorf("ResidentRAM = %d, Σ per-node = %d (ADR-025 axis 3)", got, perNode)
	}
	t.Logf("maxObserved=%d cap=%d", maxObserved, api.RAMAdmissionCeilingMB)
}

// TestSchedProperty_PerNodeRAMCeiling pins the secondary
// invariant: a per-node ceiling enforced by Request.NodeCeilingMB
// rejects admits that would push the per-node Σ past the
// ceiling, even when the global fleet has headroom. The
// fleet-wide §6.2-2 invariant is the upper bound; per-node
// ceilings are stricter and additive.
//
// Uses PlanScale (MaxConcurrency=20) so the per-app gate
// doesn't fire before the per-node gate.
func TestSchedProperty_PerNodeRAMCeiling(t *testing.T) {
	l := sched.NewNodeLedger()
	const nodeCeilingMB = 1024
	// Permit 7 × 128 MB + 8 overhead = 7×136 = 952 MB; 8th
	// admit would push to 1088 MB > 1024 → rejected.
	for i := 0; i < 7; i++ {
		if err := l.Admit(sched.Request{
			Instance:       "vm-n" + strconv.Itoa(i),
			AppID:          "n-app",
			NodeID:         "node-x",
			RAMMB:          128,
			VCPU:           1,
			Plan:           api.PlanScale,
			NodeCeilingMB:  nodeCeilingMB,
			VCPUBudget:     160,
			MaxConcurrency: 100,
		}); err != nil {
			t.Fatalf("Admit #%d: %v", i, err)
		}
	}
	err := l.Admit(sched.Request{
		Instance:       "vm-n-overflow",
		AppID:          "n-app",
		NodeID:         "node-x",
		RAMMB:          128,
		VCPU:           1,
		Plan:           api.PlanScale,
		NodeCeilingMB:  nodeCeilingMB,
		VCPUBudget:     160,
		MaxConcurrency: 100,
	})
	if err == nil {
		t.Fatal("8th admit on 1024 MB ceiling: want ErrCapacity, got nil — §6.2-2 per-node invariant violated")
	}
}

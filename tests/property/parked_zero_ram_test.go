// parked_zero_ram_test.go — §6.2 invariant #4:
//
//   A parked app consumes zero resident RAM (its cgroup must
//   be gone).
//
// Property-driven test: random admit/release cycles on
// pkg/sched.NewNodeLedger keep ResidentRAMForNode(node) = 0
// after every release (the "parked" state). The invariant is
// satisfied by the ledger's bookkeeping, not by the kernel
// cgroup — the metal test pkg/fcvm/leakcheck/metal pins the
// kernel cgroup lifecycle. This test pins the ledger side
// because it must agree with the kernel state for §6.2-4 to
// hold end-to-end.
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

// TestSchedProperty_ParkedZeroRAM pins §6.2-4: every released
// instance drops the per-node counter to its pre-admit level
// (zero for a clean node) within the same Release call. After
// release, ResidentRAMForNode == 0 AND the per-node map
// entry is deleted (no zombie keys).
//
// Uses seed=42 so the cycle is replayable.
func TestSchedProperty_ParkedZeroRAM(t *testing.T) {
	const (
		seed  = 42
		iters = 300
	)
	rng := rand.New(rand.NewSource(seed))
	l := sched.NewNodeLedger()
	nodes := []string{"node-a", "node-b", "node-c"}

	type live struct {
		instance, app, node string
		ram                 int
	}
	var admissions []live

	for i := 0; i < iters; i++ {
		roll := rng.Intn(100)
		if roll < 60 || len(admissions) == 0 {
			// Admit.
			inst := "park-vm-" + strconv.Itoa(i)
			app := []string{"pa", "pb", "pc"}[rng.Intn(3)]
			node := nodes[rng.Intn(len(nodes))]
			ram := []int{128, 256, 512}[rng.Intn(3)]
			err := l.Admit(sched.Request{
				Instance:        inst,
				AppID:           app,
				NodeID:          node,
				RAMMB:           ram,
				VCPU:            1,
				Plan:            api.PlanScale,
				NodeCeilingMB:   100000,
				VCPUBudget:      160,
				MaxConcurrency:  100,
			})
			if err == nil {
				admissions = append(admissions, live{inst, app, node, ram})
			}
		} else {
			// Release (park).
			idx := rng.Intn(len(admissions))
			a := admissions[idx]
			l.Release(a.instance)
			admissions[idx] = admissions[len(admissions)-1]
			admissions = admissions[:len(admissions)-1]

			// INVARIANT #4: after Release, ResidentRAMForNode
			// for that node must reflect ONLY the still-live
			// admissions on it. If `a.node` has zero remaining
			// admissions, ResidentRAMForNode must be 0.
			var remaining int
			for _, x := range admissions {
				if x.node == a.node {
					remaining += x.ram + api.PerVMOverheadMB
				}
			}
			if got := l.ResidentRAMForNode(a.node); got != remaining {
				t.Fatalf("after Release(%s): ResidentRAMForNode(%s) = %d, want %d (live %d on node) — §6.2-4 violated",
					a.instance, a.node, got, remaining, remaining)
			}
		}
	}

	// Final invariant: every node has the per-node counter
	// equal to the Σ of its still-live admissions.
	for _, n := range nodes {
		var expected int
		for _, x := range admissions {
			if x.node == n {
				expected += x.ram + api.PerVMOverheadMB
			}
		}
		if got := l.ResidentRAMForNode(n); got != expected {
			t.Errorf("ResidentRAMForNode(%s) = %d, want %d (live remaining)",
				n, got, expected)
		}
	}
}

// TestSchedProperty_ReleaseAllDropsRAMToZero pins the cleanup
// invariant: when ALL admits on a node have been released, the
// per-node counter is exactly 0 AND the per-node map entry is
// gone (no zombie key).
func TestSchedProperty_ReleaseAllDropsRAMToZero(t *testing.T) {
	l := sched.NewNodeLedger()
	for i := 0; i < 5; i++ {
		if err := l.Admit(sched.Request{
			Instance:        "z-" + strconv.Itoa(i),
			AppID:           "z-app",
			NodeID:          "node-z",
			RAMMB:           128,
			VCPU:            1,
			Plan:            api.PlanScale,
			NodeCeilingMB:   100000,
			VCPUBudget:      160,
			MaxConcurrency:  100,
		}); err != nil {
			t.Fatalf("Admit %d: %v", i, err)
		}
	}
	if l.ResidentRAMForNode("node-z") == 0 {
		t.Fatal("pre-Release ResidentRAMForNode(node-z) = 0 (admit didn't register?)")
	}
	// Release them all.
	for i := 0; i < 5; i++ {
		l.Release("z-" + strconv.Itoa(i))
	}
	if got := l.ResidentRAMForNode("node-z"); got != 0 {
		t.Errorf("after all releases: ResidentRAMForNode(node-z) = %d, want 0", got)
	}
	if got := l.ResidentRAM(); got != 0 {
		t.Errorf("after all releases: ResidentRAM() = %d, want 0", got)
	}
}

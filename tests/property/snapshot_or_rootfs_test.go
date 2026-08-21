// snapshot_or_rootfs_test.go — §6.2 invariant #3:
//
//   An app always has a live snapshot OR a cold-bootable
//   rootfs — never neither (ADR-005: snapshots are cache, not
//   truth; the rootfs is the load-bearing artefact).
//
// Property-driven test: random walks through the canonical
// state transitions never leave the (snapshot, rootfs)
// pair in the (false, false) invalid state. The test models
// the scheduler-side bookkeeping (which commands the vmmd
// state transitions) without touching real Firecracker.
//
// State machine modeled (per ADR-005 + ADR-023):
//
//	DEPLOY              — both snapshot + rootfs present
//	WAKE                — normal wake; consumes snapshot at restore
//	PARK                — VM parked; writes a fresh snapshot
//	SNAPSHOT_STALE      — FC upgrade invalidated snapshot; rootfs only
//	RE_SNAPSHOT         — successful re-snapshot from rootfs
//	ROOTFS_CATASTROPHE  — node rebuild dropped rootfs; snapshot present
//
// apply() returns:
//   - nil: post-state satisfies §6.2-3 (the invariant).
//   - errPrecondition (sentinel): the transition's pre-state
//     is incompatible with the operation. NOT a §6.2-3
//     violation — a precondition error means the caller
//     acted on a state where the operation was never
//     legal in the first place (caller bug, not invariant
//     bug). The state is NOT mutated on this branch.
//   - any other error: post-state violates §6.2-3 (snapshot
//     OR rootfs), the actual invariant violation we test for.
//
// Whitebox test (package property).
package property

import (
	"errors"
	"math/rand"
	"testing"
)

// errPrecondition is the sentinel for "transition not
// applicable from this state". apply() returns
// errors.Is(err, errPrecondition) on this branch so the
// caller (test) can distinguish "caller tried an
// illegal transition" from "post-state violates §6.2-3".
var errPrecondition = errors.New("precondition not met")

// appState is the per-app state book kept by schedd.
type appState struct {
	id          string
	hasSnapshot bool
	hasRootfs   bool
}

// invariantErr returns a §6.2-3 violation error (or nil).
func (a *appState) invariantErr() error {
	if !a.hasSnapshot && !a.hasRootfs {
		return errors.New("app " + a.id + ": (snapshot=false, rootfs=false) — §6.2-3 violated")
	}
	return nil
}

// startErr returns an error iff the start state is
// (false, false). Production schedd refuses to operate on
// an app in that state (the operator must DEPLOY first);
// this mirrors that contract.
func (a *appState) startErr() error {
	if !a.hasSnapshot && !a.hasRootfs {
		return errors.New("app " + a.id + ": (false, false) start state — invalid; DEPLOY first")
	}
	return nil
}

// apply runs a transition. Returns:
//   - nil if the post-state satisfies §6.2-3.
//   - errPrecondition (wrapped) if the transition is not
//     applicable from this state — NOT an invariant
//     violation.
//   - a non-precondition error if the post-state violates
//     §6.2-3 (the actual bug).
//
// apply() is idempotent on precondition failures (state
// unchanged).
func (a *appState) apply(t string) error {
	if err := a.startErr(); err != nil {
		return err // start state itself is invalid (not a transition error)
	}
	switch t {
	case "DEPLOY":
		// Always succeeds (recovery).
		a.hasSnapshot, a.hasRootfs = true, true
		return a.invariantErr()
	case "WAKE":
		// requires both; snapshot consumed at restore.
		if !a.hasSnapshot || !a.hasRootfs {
			return errPrecondition
		}
		a.hasSnapshot = false
		return a.invariantErr()
	case "PARK":
		// requires rootfs (the live VM has it). Writes a fresh
		// snapshot.
		if !a.hasRootfs {
			return errPrecondition
		}
		a.hasSnapshot, a.hasRootfs = true, true
		return a.invariantErr()
	case "SNAPSHOT_STALE":
		// FC upgrade invalidated snapshot; rootfs preserved.
		if !a.hasSnapshot || !a.hasRootfs {
			return errPrecondition
		}
		a.hasSnapshot = false
		return a.invariantErr()
	case "RE_SNAPSHOT":
		// re-snapshot from rootfs (the only source).
		if !a.hasRootfs {
			return errPrecondition
		}
		a.hasSnapshot = true
		return a.invariantErr()
	case "ROOTFS_CATASTROPHE":
		// node rebuild dropped the rootfs layer; snapshot
		// survives as the only artefact.
		if !a.hasSnapshot || !a.hasRootfs {
			return errPrecondition
		}
		a.hasRootfs = false
		a.hasSnapshot = true
		return a.invariantErr()
	}
	return errors.New("unknown transition: " + t)
}

// TestSchedProperty_SnapshotOrRootfs pins §6.2-3 across
// 300 random transitions per app with seed=42. Precondition
// rejections are expected (caller did an illegal thing);
// actual invariant violations fail the test.
func TestSchedProperty_SnapshotOrRootfs(t *testing.T) {
	const (
		seed  = 42
		iters = 300
	)
	rng := rand.New(rand.NewSource(seed))
	transitions := []string{
		"DEPLOY", "WAKE", "PARK",
		"SNAPSHOT_STALE", "RE_SNAPSHOT", "ROOTFS_CATASTROPHE",
	}
	cases := []struct {
		name        string
		startSnap   bool
		startRootfs bool
	}{
		{"only-rootfs", false, true},
		{"only-snapshot", true, false},
		{"both-present", true, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			a := appState{id: "app-" + tc.name, hasSnapshot: tc.startSnap, hasRootfs: tc.startRootfs}
			if err := a.startErr(); err != nil {
				t.Fatalf("start state invalid: %v", err)
			}
			for i := 0; i < iters; i++ {
				t1 := transitions[rng.Intn(len(transitions))]
				err := a.apply(t1)
				if errors.Is(err, errPrecondition) {
					continue // expected: caller tried an illegal transition; state unchanged
				}
				if err != nil {
					t.Fatalf("iteration %d, transition %s: %v — §6.2-3 violated (start: snap=%v, rootfs=%v)",
						i, t1, err, tc.startSnap, tc.startRootfs)
				}
			}
		})
	}
}

// TestSchedProperty_NeitherStateUnreachable pins the
// transition-graph invariant: no sequence of two transitions
// from a non-(false,false) start leaves the state in
// (false, false). Precondition rejections are tolerated.
func TestSchedProperty_NeitherStateUnreachable(t *testing.T) {
	transitions := []string{
		"DEPLOY", "WAKE", "PARK",
		"SNAPSHOT_STALE", "RE_SNAPSHOT", "ROOTFS_CATASTROPHE",
	}
	states := []struct {
		snap, rootfs bool
	}{
		{true, true},
		{true, false},
		{false, true},
	}
	for _, start := range states {
		for _, t1 := range transitions {
			for _, t2 := range transitions {
				a := appState{id: "app-x", hasSnapshot: start.snap, hasRootfs: start.rootfs}
				err := a.apply(t1)
				if errors.Is(err, errPrecondition) {
					continue
				}
				if err != nil {
					t.Errorf("start=(snap=%v,rootfs=%v) -> %s: %v",
						start.snap, start.rootfs, t1, err)
					continue
				}
				err = a.apply(t2)
				if errors.Is(err, errPrecondition) {
					continue
				}
				if err != nil {
					t.Errorf("start=(snap=%v,rootfs=%v) -> %s -> %s: %v",
						start.snap, start.rootfs, t1, t2, err)
				}
			}
		}
	}
}

// TestSchedProperty_DEPLOYAlwaysResets is a contract pin: the
// DEPLOY transition always recovers any valid start state to
// (true, true).
func TestSchedProperty_DEPLOYAlwaysResets(t *testing.T) {
	for _, start := range []struct {
		snap, rootfs bool
	}{
		{true, true}, {true, false}, {false, true},
	} {
		a := appState{id: "app-r", hasSnapshot: start.snap, hasRootfs: start.rootfs}
		if err := a.apply("DEPLOY"); err != nil {
			t.Errorf("DEPLOY from (snap=%v, rootfs=%v): %v", start.snap, start.rootfs, err)
		}
		if !a.hasSnapshot || !a.hasRootfs {
			t.Errorf("DEPLOY from (snap=%v, rootfs=%v): post=(snap=%v, rootfs=%v); want both true",
				start.snap, start.rootfs, a.hasSnapshot, a.hasRootfs)
		}
	}
}

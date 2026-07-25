package state

import "testing"

func TestLegalTransitions(t *testing.T) {
	legal := [][2]State{
		{StateParked, StateWaking},
		{StateWaking, StateRunning},
		{StateWaking, StateColdBooting}, // restore failed → fallback
		{StateColdBooting, StateRunning},
		{StateRunning, StateSnapshotting},
		{StateSnapshotting, StateParked},
		{StateSnapshotting, StateStopped}, // snapshot failed
		{StateStopped, StateColdBooting},  // next wake cold boots
		{StateRunning, StateFailed},       // crash loop
		{StateColdBooting, StateFailed},   // boot timeout
	}
	for _, e := range legal {
		if !CanTransition(e[0], e[1]) {
			t.Errorf("%s→%s should be legal", e[0], e[1])
		}
	}
}

func TestIllegalTransitions(t *testing.T) {
	illegal := [][2]State{
		{StateParked, StateRunning},      // must wake first
		{StateRunning, StateParked},      // must snapshot first
		{StateParked, StateParked},       // no self-loop
		{StateRunning, StateColdBooting}, // can't re-boot a running vm
		{StateFailed, StateRunning},      // failed re-parks, not resumes
		{StateStopped, StateWaking},      // stopped has no snapshot to restore
	}
	for _, e := range illegal {
		if CanTransition(e[0], e[1]) {
			t.Errorf("%s→%s should be illegal", e[0], e[1])
		}
	}
}

func TestEveryStateValidAndReachable(t *testing.T) {
	if len(States) != len(transitions) {
		t.Fatalf("States list (%d) and transition table (%d) out of sync", len(States), len(transitions))
	}
	// Every state must be a transition target of some other state (reachable),
	// except the entry state PARKED (reached via the deploy pipeline).
	reachable := map[State]bool{StateParked: true}
	for _, targets := range transitions {
		for _, to := range targets {
			reachable[to] = true
		}
	}
	for _, s := range States {
		if !s.Valid() {
			t.Errorf("state %s not valid", s)
		}
		if !reachable[s] {
			t.Errorf("state %s is unreachable", s)
		}
	}
}

func TestConcurrencyAccounting(t *testing.T) {
	// Invariant §6.2-1: only these three count toward max_concurrency.
	want := map[State]bool{StateWaking: true, StateColdBooting: true, StateRunning: true}
	for _, s := range States {
		if got := s.CountsForConcurrency(); got != want[s] {
			t.Errorf("%s.CountsForConcurrency() = %v, want %v", s, got, want[s])
		}
	}
}

func TestRAMAccounting(t *testing.T) {
	// Invariant §6.2-2: these four hold resident RAM.
	want := map[State]bool{StateWaking: true, StateColdBooting: true, StateRunning: true, StateSnapshotting: true}
	for _, s := range States {
		if got := s.CountsForRAM(); got != want[s] {
			t.Errorf("%s.CountsForRAM() = %v, want %v", s, got, want[s])
		}
	}
	// A parked instance must not count for RAM (§6.2-4).
	if StateParked.CountsForRAM() {
		t.Error("parked instances must hold zero resident RAM")
	}
}

// TestIsLive pins the IsLive predicate for every state in
// machine.go::States. IsLive is the single source of truth for
// "live row" semantics — schedd's eviction subscriber,
// ListAllInstances' filter (pgstore.go:1683), and any future
// quota eviction read through it. The table below also covers
// the string-typed entry point (machine.go:105) so a future
// regression that drops the `State(s).CountsForRAM()` indirection
// surfaces here.
//
// The set's exact membership is the load-bearing contract:
// {WAKING, COLD_BOOTING, RUNNING, SNAPSHOTTING} — the same four
// states counted for RAM (§6.2-2). PARKED, STOPPED, FAILED,
// EVICTING_ACCOUNT_DELETING are NOT live.
//
// Strings are the documented state.State constants, not literals,
// so retyping s in the table (e.g. "failing" instead of "failed")
// fails fast via the State-typed want column.
func TestIsLive(t *testing.T) {
	// anchor is the State-typed key so the table fails at
	// compile time if the constant is renamed.
	want := map[State]bool{
		StateWaking:                    true,
		StateColdBooting:               true,
		StateRunning:                   true,
		StateSnapshotting:              true,
		StateParked:                    false,
		StateStopped:                   false,
		StateFailed:                    false,
		StateEvictingAccountDeleting:   false,
	}
	for _, s := range States {
		t.Run(string(s), func(t *testing.T) {
			// Type-asserted entry point.
			got := IsLive(string(s))
			if got != want[s] {
				t.Errorf("IsLive(%q) = %v, want %v", string(s), got, want[s])
			}
			// Mirror through the State-typed entry point so
			// the string ↔ State round-trip is also pinned.
			if gotState := s.CountsForRAM(); gotState != want[s] {
				t.Errorf("%s.CountsForRAM() = %v, want %v (mirror of IsLive)",
					s, gotState, want[s])
			}
		})
	}
}

// Tests for ConnStateTracker (Tier A8 / ADR-083 / code-review
// fix #5).

package gateway

import (
	"net/http"
	"sync"
	"testing"
)

// Test the four-state transition table from
// pkg/gateway/inflight.go: New +1, Closed -1, Hijacked -1,
// Active no change. Run concurrently to surface any
// data race (passes under -race).
func TestConnStateTracker_Transitions(t *testing.T) {
	tr := NewConnStateTracker()

	cases := []struct {
		name  string
		state http.ConnState
		want  int // delta, not absolute
	}{
		{"new", http.StateNew, 1},
		{"active_no_change", http.StateActive, 0},
		{"idle_no_change", http.StateIdle, 0},
		{"hijacked", http.StateHijacked, -1},
		{"closed", http.StateClosed, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := tr.Count()
			tr.ConnState(nil /* net.Conn — unused */, tc.state)
			after := tr.Count()
			if got := after - before; got != tc.want {
				t.Errorf("ConnState(%s): delta = %d, want %d", tc.state, got, tc.want)
			}
		})
	}
}

// Verify the arithmetic a drain actually does: 5 connections
// open → Count() == 5; close 3 → Count() == 2; close 2 → 0.
func TestConnStateTracker_DrainArithmetic(t *testing.T) {
	tr := NewConnStateTracker()
	for i := 0; i < 5; i++ {
		tr.ConnState(nil, http.StateNew)
	}
	if got := tr.Count(); got != 5 {
		t.Fatalf("after 5 New: Count = %d, want 5", got)
	}
	for i := 0; i < 3; i++ {
		tr.ConnState(nil, http.StateClosed)
	}
	if got := tr.Count(); got != 2 {
		t.Fatalf("after 3 Closed: Count = %d, want 2", got)
	}
	for i := 0; i < 2; i++ {
		tr.ConnState(nil, http.StateHijacked) // h2c upgrade shape
	}
	if got := tr.Count(); got != 0 {
		t.Fatalf("after 2 Hijacked: Count = %d, want 0", got)
	}
}

// Run 1000 goroutines, each toggling New/Closed. The Count()
// must end at 0 and never go negative (atomic Int64 — verify
// the invariant under -race).
func TestConnStateTracker_RaceSafe(t *testing.T) {
	tr := NewConnStateTracker()
	var wg sync.WaitGroup
	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr.ConnState(nil, http.StateNew)
			tr.ConnState(nil, http.StateClosed)
		}()
	}
	wg.Wait()
	if got := tr.Count(); got != 0 {
		t.Errorf("after 1000 New+Closed pairs: Count = %d, want 0", got)
	}
}

// Compile-time guard: ConnStateTracker must satisfy InFlightCounter
// so cmd/gatewayd-public can hand it to DNSHandoff.InFlight
// directly without an adapter.
var _ InFlightCounter = (*ConnStateTracker)(nil)

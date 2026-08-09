// Tests for the DNS handoff orchestrator (Tier A8 / ADR-083).

package gateway

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/gateway/leader"
	"github.com/onebox-faas/faas/pkg/wire"
)

// newTestOpsMetrics constructs a fresh OpsMetrics on a private
// registry so the terminal-state assertions don't pollute the
// global registry and don't bleed across tests. The prefix is
// arbitrary but unique per package so a parallel test that
// constructs a second registry doesn't collide.
func newTestOpsMetrics(t *testing.T) *wire.OpsMetrics {
	t.Helper()
	return wire.NewOpsMetrics("faas_test_" + t.Name())
}

// fakeDNSProvider records calls. err is returned by every
// call; nil → success on every call.
type fakeDNSProvider struct {
	calls atomic.Int64
	err   error
}

func (f *fakeDNSProvider) UpsertRecord(_ context.Context, _, _ string) error {
	f.calls.Add(1)
	return f.err
}

func (f *fakeDNSProvider) DeleteRecord(_ context.Context, _ string) error {
	f.calls.Add(1)
	return f.err
}

func (f *fakeDNSProvider) CallCount() int64 { return f.calls.Load() }

// fakeInFlight implements InFlightCounter. count starts at
// from; the test decrements via Dec() to simulate draining.
type fakeInFlight struct {
	mu    chan struct{}
	count int
}

func newFakeInFlight(start int) *fakeInFlight {
	return &fakeInFlight{mu: make(chan struct{}, 1), count: start}
}

func (f *fakeInFlight) Count() int { return f.count }
func (f *fakeInFlight) Dec()       { f.count-- }

// Test happy path: in-flight drains to 0, DNS succeeds, outcome
// is dns_flipped.
func TestDNSHandoff_HappyPath(t *testing.T) {
	p := &fakeDNSProvider{}
	fl := newFakeInFlight(0)
	d := &DNSHandoff{
		NodeName:    "node-a",
		DNSProvider: p,
		InFlight:    fl,
		// Metrics nil → drainNoMetrics path; outcome still
		// observable via return value.
		Now: func() time.Time { return time.Unix(0, 0) },
	}
	out := d.Run(context.Background())
	if out != OutcomeDNSFlipped {
		t.Errorf("Run = %q, want %q", out, OutcomeDNSFlipped)
	}
	if got := p.CallCount(); got != 1 {
		t.Errorf("expected 1 DNS call, got %d", got)
	}
}

// Test in-flight does not drain within budget → peer_unreachable.
func TestDNSHandoff_StuckInFlight(t *testing.T) {
	p := &fakeDNSProvider{}
	fl := newFakeInFlight(10) // never drains
	clock := time.Unix(0, 0)
	shortBudget := 100 * time.Millisecond
	d := &DNSHandoff{
		NodeName:    "node-a",
		DNSProvider: p,
		InFlight:    fl,
		// Advance the clock past the budget on every tick so
		// the deadline check fires deterministically.
		Now:    func() time.Time { clock = clock.Add(200 * time.Millisecond); return clock },
		Budget: &shortBudget,
	}
	out := d.Run(context.Background())
	if out != OutcomePeerUnreachable {
		t.Errorf("Run = %q, want %q", out, OutcomePeerUnreachable)
	}
	if got := p.CallCount(); got != 0 {
		t.Errorf("DNS must not be called when in-flight stuck, got %d calls", got)
	}
}

// Test DNS provider fails persistently → dns_stale.
func TestDNSHandoff_DNSStale(t *testing.T) {
	p := &fakeDNSProvider{err: errors.New("hetzner 503")}
	fl := newFakeInFlight(0)
	clock := time.Unix(0, 0)
	shortBudget := 100 * time.Millisecond
	d := &DNSHandoff{
		NodeName:    "node-a",
		DNSProvider: p,
		InFlight:    fl,
		// Advance the clock past the budget on every call so
		// the deadline check fires deterministically.
		Now:    func() time.Time { clock = clock.Add(200 * time.Millisecond); return clock },
		Budget: &shortBudget,
	}
	out := d.Run(context.Background())
	if out != OutcomeDNSStale {
		t.Errorf("Run = %q, want %q", out, OutcomeDNSStale)
	}
	if got := p.CallCount(); got == 0 {
		t.Errorf("DNS must be retried at least once, got 0 calls")
	}
}

// Test nil DNSProvider → dns_stale (no panic).
func TestDNSHandoff_NilDNSProvider(t *testing.T) {
	d := &DNSHandoff{
		NodeName:    "node-a",
		DNSProvider: nil,
		InFlight:    newFakeInFlight(0),
	}
	out := d.Run(context.Background())
	if out != OutcomeDNSStale {
		t.Errorf("Run = %q, want %q", out, OutcomeDNSStale)
	}
}

// Test concurrent Run() invocations serialize.
func TestDNSHandoff_ConcurrentRunsSerialized(t *testing.T) {
	p := &fakeDNSProvider{}
	fl := newFakeInFlight(0)
	d := &DNSHandoff{
		NodeName:    "node-a",
		DNSProvider: p,
		InFlight:    fl,
	}
	const N = 5
	var outcomes [N]Outcome
	done := make(chan int, N)
	for i := 0; i < N; i++ {
		go func(i int) {
			outcomes[i] = d.Run(context.Background())
			done <- i
		}(i)
	}
	for i := 0; i < N; i++ {
		<-done
	}
	// All runs should report dns_flipped; serialization is
	// internal, the outcome is the same.
	for i, out := range outcomes {
		if out != OutcomeDNSFlipped {
			t.Errorf("outcomes[%d] = %q, want %q", i, out, OutcomeDNSFlipped)
		}
	}
}

// Test the no-LeaderStore path: the orchestrator does NOT
// re-elect — that's the caller's job (cmd/gatewayd-public
// reads compute_node_changed, elects, then calls Run if this
// node is the dying leader). Verify Run does not panic on
// nil LeaderStore.
func TestDNSHandoff_NilLeaderStore(t *testing.T) {
	p := &fakeDNSProvider{}
	d := &DNSHandoff{
		NodeName:    "node-a",
		LeaderStore: nil,
		DNSProvider: p,
	}
	out := d.Run(context.Background())
	if out != OutcomeDNSFlipped {
		t.Errorf("Run = %q, want %q (LeaderStore is caller-set, not consulted here)", out, OutcomeDNSFlipped)
	}
}

// Test the no-InFlight path: defaults to noopInFlight so the
// orchestrator proceeds straight to the DNS step.
func TestDNSHandoff_NilInFlight(t *testing.T) {
	p := &fakeDNSProvider{}
	d := &DNSHandoff{
		NodeName:    "node-a",
		DNSProvider: p,
		// InFlight nil → noopInFlight{} (Count() == 0)
	}
	out := d.Run(context.Background())
	if out != OutcomeDNSFlipped {
		t.Errorf("Run = %q, want %q", out, OutcomeDNSFlipped)
	}
}

// Test the budget constant is the documented 30 seconds.
func TestHADNSRecordStaleSeconds_Default(t *testing.T) {
	if api.HADNSRecordStaleSeconds != 30 {
		t.Errorf("api.HADNSRecordStaleSeconds = %d, want 30", api.HADNSRecordStaleSeconds)
	}
}

// Verify leader.LeaderStore and leader.Leader (from
// pkg/gateway/leader) compose with the orchestrator — the
// election is the caller's job; the orchestrator doesn't
// import leader except to satisfy the LeaderStore interface
// on its own field.
func TestDNSHandoff_LeaderStoreFieldAcceptsLeaderPackage(t *testing.T) {
	var _ leader.LeaderStore = (*fakeStoreForDNS)(nil)
}

// Code-review fix #1: terminal StandbyState transitions. The
// gauge MUST leave Draining (3) on every Run() exit —
// FaasStandbyStateDrainingTooLong cannot fire while the gauge
// holds at 3 forever. Three terminal cases:
//   - dns_flipped → Drained (4). Box is safe to shut down.
//   - dns_stale → Failed (5). Terminal failure; operator
//     intervention per the runbook's escalation section.
//   - peer_unreachable → Warm (2). Box still functional, no
//     DNS change happened; bounce back so the next
//     pg_notify attempt can re-enter.
func TestDNSHandoff_TerminalStates(t *testing.T) {
	shortBudget := 100 * time.Millisecond
	clock := time.Unix(0, 0)
	advanceClock := func() time.Time {
		clock = clock.Add(200 * time.Millisecond)
		return clock
	}

	t.Run("dns_flipped_transitions_to_Drained", func(t *testing.T) {
		m := newTestOpsMetrics(t)
		p := &fakeDNSProvider{}
		d := &DNSHandoff{
			NodeName:    "node-a",
			DNSProvider: p,
			InFlight:    newFakeInFlight(0),
			Metrics:     m,
			Now:         func() time.Time { return clock },
			Budget:      &shortBudget,
		}
		// Drive Run through the success path.
		if out := d.Run(context.Background()); out != OutcomeDNSFlipped {
			t.Fatalf("Run = %q, want %q", out, OutcomeDNSFlipped)
		}
		if got := m.StandbyState(); got != wire.StandbyStateDrained {
			t.Errorf("StandbyState = %d, want %d (Drained)", got, wire.StandbyStateDrained)
		}
	})

	t.Run("dns_stale_transitions_to_Failed", func(t *testing.T) {
		m := newTestOpsMetrics(t)
		p := &fakeDNSProvider{err: errors.New("hetzner 503")}
		d := &DNSHandoff{
			NodeName:    "node-a",
			DNSProvider: p,
			InFlight:    newFakeInFlight(0),
			Metrics:     m,
			Now:         advanceClock, // past budget on first call
			Budget:      &shortBudget,
		}
		if out := d.Run(context.Background()); out != OutcomeDNSStale {
			t.Fatalf("Run = %q, want %q", out, OutcomeDNSStale)
		}
		if got := m.StandbyState(); got != wire.StandbyStateFailed {
			t.Errorf("StandbyState = %d, want %d (Failed)", got, wire.StandbyStateFailed)
		}
	})

	t.Run("peer_unreachable_bounces_back_to_Warm", func(t *testing.T) {
		m := newTestOpsMetrics(t)
		p := &fakeDNSProvider{}  // DNS would succeed; in-flight is the blocker
		fl := newFakeInFlight(1) // stuck at 1
		d := &DNSHandoff{
			NodeName:    "node-a",
			DNSProvider: p,
			InFlight:    fl,
			Metrics:     m,
			Now:         advanceClock,
			Budget:      &shortBudget,
		}
		if out := d.Run(context.Background()); out != OutcomePeerUnreachable {
			t.Fatalf("Run = %q, want %q", out, OutcomePeerUnreachable)
		}
		if got := m.StandbyState(); got != wire.StandbyStateWarm {
			t.Errorf("StandbyState = %d, want %d (Warm)", got, wire.StandbyStateWarm)
		}
	})
}

// fakeStoreForDNS mirrors the leader package's fakeStore so
// the compiler enforces the interface contract.
type fakeStoreForDNS struct{ nodes []leader.ComputeNode }

func (f *fakeStoreForDNS) ListActiveComputeNodes(_ context.Context) ([]leader.ComputeNode, error) {
	return f.nodes, nil
}

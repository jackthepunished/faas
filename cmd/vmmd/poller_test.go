package main

// PR-E egress-deny counter poll adapter unit tests.
//
// The tests exercise runEgressPoll directly with a stubbed
// popCountersFunc so the delta-tracking logic is provable without
// /dev/kvm and without shelling out to nft. The non-metal build of
// pkg/netns.PopCounters is a stub returning an empty map, so the
// poller naturally tests as "no drops surfaced" — but we need
// DETERMINISTIC proof that a 0→5→10 sequence surfaces as 5+5=10,
// not 5+10=15 (the Add(-5) panic case).
//
// The poller is also tested for the "counter reset" path (nft
// table flush, snapshot resume) — lastSeen must be re-seeded and
// no negative delta emitted.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/wire"
)

// fakePopCounters is a thread-safe popCountersFunc stub. Each call
// increments a counter and returns the current snapshot — tests
// drive the sequence by appending to `values` before the poller
// reads.
type fakePopCounters struct {
	mu     sync.Mutex
	values []map[string]uint64
	calls  atomic.Int32
	err    error
}

func (f *fakePopCounters) pop(ctx context.Context) (map[string]uint64, error) {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	// Round-robin the queued snapshots; callers stack the values
	// they want each successive poll to return.
	i := int(f.calls.Load()) - 1
	if i >= len(f.values) {
		i = len(f.values) - 1
	}
	if i < 0 {
		return map[string]uint64{}, nil
	}
	out := make(map[string]uint64, len(f.values[i]))
	for k, v := range f.values[i] {
		out[k] = v
	}
	return out, nil
}

// TestRunEgressPoll_PrimedOnFirstTick (PR-E) — the first poll
// must NOT emit any delta (it only seeds lastSeen). Without this,
// every vmmd restart would surface a "0 → curr" spike right after
// boot — false-positive alert.
func TestRunEgressPoll_PrimedOnFirstTick(t *testing.T) {
	fp := &fakePopCounters{values: []map[string]uint64{
		{"drop_v4_10_0_0_0_8": 100, "drop_v6_fe80___10": 50},
		{"drop_v4_10_0_0_0_8": 100, "drop_v6_fe80___10": 50},
	}}
	ops := wire.NewOpsMetrics("vmmd")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runEgressPoll(ctx, ops, fp.pop, 5*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()

	// Wait long enough for at least 2 ticks.
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	// Both entries had zero delta (100→100, 50→50) so no Add fired.
	body := renderMetrics(t, ops)
	// Per-cidr, per-family zero-value pre-instantiated lines.
	cases := []struct{ cidr, family string }{
		{"drop_v4_10_0_0_0_8", "ip"},
		{"drop_v6_fe80___10", "ip6"},
	}
	for _, c := range cases {
		needle := `vmmd_egress_deny_total{cidr="` + c.cidr + `",family="` + c.family + `"} 0`
		if !containsLine(body, needle) {
			t.Errorf("expected primed (no emit) state for %s; body:\n%s", needle, body)
		}
	}
}

// TestRunEgressPoll_DeltaOnSecondTick (PR-E) — a 100→105
// sequence on the second poll surfaces as vmmd_egress_deny_total = 5
// (delta), not 105 (absolute). The first tick seeds lastSeen;
// the second tick emits the delta.
func TestRunEgressPoll_DeltaOnSecondTick(t *testing.T) {
	fp := &fakePopCounters{values: []map[string]uint64{
		{"drop_v4_10_0_0_0_8": 100},
		{"drop_v4_10_0_0_0_8": 105},
		{"drop_v4_10_0_0_0_8": 113}, // third tick → delta 8
	}}
	ops := wire.NewOpsMetrics("vmmd")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runEgressPoll(ctx, ops, fp.pop, 5*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()

	time.Sleep(80 * time.Millisecond)
	cancel()
	<-done

	body := renderMetrics(t, ops)
	// 5 + 8 = 13 cumulative increments on this counter.
	want := `vmmd_egress_deny_total{cidr="drop_v4_10_0_0_0_8",family="ip"} 13`
	if !containsLine(body, want) {
		t.Errorf("expected %q; body:\n%s", want, body)
	}
}

// TestRunEgressPoll_CounterReset (PR-E) — when curr < prev (nft
// table flush / manual `nft reset counters`), the poller must
// NOT emit a negative delta. lastSeen is re-seeded to curr and
// the loop continues from there.
func TestRunEgressPoll_CounterReset(t *testing.T) {
	fp := &fakePopCounters{values: []map[string]uint64{
		{"drop_v4_10_0_0_0_8": 100},
		{"drop_v4_10_0_0_0_8": 105}, // delta +5
		{"drop_v4_10_0_0_0_8": 0},   // reset (curr < prev)
		{"drop_v4_10_0_0_0_8": 7},   // delta +7 from the reset baseline
	}}
	ops := wire.NewOpsMetrics("vmmd")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runEgressPoll(ctx, ops, fp.pop, 5*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	// 5 + 7 = 12 cumulative. The reset (curr=0 after prev=105) must
	// not contribute a negative value.
	body := renderMetrics(t, ops)
	want := `vmmd_egress_deny_total{cidr="drop_v4_10_0_0_0_8",family="ip"} 12`
	if !containsLine(body, want) {
		t.Errorf("expected %q; body:\n%s", want, body)
	}
}

// TestRunEgressPoll_PopCountersError (PR-E) — a poll failure logs
// at Warn and is skipped (lastSeen untouched, no metric emitted).
// The next successful poll must NOT carry over a delta that
// includes the failed interval.
func TestRunEgressPoll_PopCountersError(t *testing.T) {
	// First call succeeds (seed), second errors, third succeeds
	// (delta from the SEED value, not from the errored tick).
	fp := &errPopCounters{
		values: []map[string]uint64{
			{"drop_v4_10_0_0_0_8": 100}, // tick 1: seed
			nil,                         // tick 2: error — should be skipped
			{"drop_v4_10_0_0_0_8": 110}, // tick 3: success → delta 10 from seed
		},
		errAt: 1,
	}
	ops := wire.NewOpsMetrics("vmmd")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runEgressPoll(ctx, ops, fp.pop, 5*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	body := renderMetrics(t, ops)
	want := `vmmd_egress_deny_total{cidr="drop_v4_10_0_0_0_8",family="ip"} 10`
	if !containsLine(body, want) {
		t.Errorf("expected %q (delta from seed, not from errored tick); body:\n%s", want, body)
	}
}

type errPopCounters struct {
	mu     sync.Mutex
	values []map[string]uint64
	errAt  int
	calls  atomic.Int32
}

func (e *errPopCounters) pop(ctx context.Context) (map[string]uint64, error) {
	n := int(e.calls.Add(1))
	e.mu.Lock()
	defer e.mu.Unlock()
	if n-1 == e.errAt {
		return nil, errors.New("simulated nft failure")
	}
	i := n - 1
	if i > len(e.values)-1 {
		i = len(e.values) - 1
	}
	if i < 0 {
		return map[string]uint64{}, nil
	}
	out := make(map[string]uint64, len(e.values[i]))
	for k, v := range e.values[i] {
		out[k] = v
	}
	return out, nil
}

// renderMetrics is a test helper that serves the metrics registry
// over a local httptest server and returns the body. Mirrors the
// helper in pkg/wire/metrics_test.go.
func renderMetrics(t *testing.T, ops *wire.OpsMetrics) string {
	t.Helper()
	srv := httptest.NewServer(ops.Handler())
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(body)
}

func containsLine(body, needle string) bool {
	for _, line := range splitLines(body) {
		if line == needle {
			return true
		}
	}
	return false
}

func splitLines(s string) []string {
	var out []string
	cur := ""
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, cur)
			cur = ""
		} else {
			cur += string(s[i])
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

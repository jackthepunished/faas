// pure_helpers_test.go — fill pkg/meter coverage of the tiny
// pure helpers and the simple setters reachable without
// spinning up a Postgres pool, a Stripe/Paddle sandbox, or
// a running Meterd loop.
//
// Targets:
//   - CPUHours (the usec → hour arithmetic)
//   - Loop.WithEgress / WithProbe / WithPartitionCreate (setters)
//
// Whitebox `package meter`.
package meter

import (
	"context"
	"testing"
)

// --- CPUHours --------------------------------------------------

// CPUHours converts microseconds to hours. 1 hour = 3.6e9 µs.
// Pinned at canonical inputs so the wire shape (UsageResponse.CPUHours)
// doesn't drift from the billing unit (plan RAM, never CPU).
func TestCPUHours(t *testing.T) {
	cases := []struct {
		name string
		usec int64
		want float64
	}{
		// Zero baseline.
		{name: "zero", usec: 0, want: 0.0},
		// One second of CPU = 1e6 µs.
		{name: "one_second", usec: 1_000_000, want: 1.0 / 3600.0},
		// One hour of CPU = 3.6e9 µs exactly.
		{name: "one_hour", usec: 3_600_000_000, want: 1.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CPUHours(tc.usec)
			if got != tc.want {
				t.Errorf("CPUHours(%d) = %v, want %v", tc.usec, got, tc.want)
			}
		})
	}
}

// --- Loop setters ---------------------------------------------

func TestLoop_WithEgress_SetsAndReturnsReceiver(t *testing.T) {
	l := &Loop{}
	var src EgressSource = nil
	if got := l.WithEgress(src); got != l {
		t.Error("WithEgress did not return receiver")
	}
	if l.egress != src {
		t.Errorf("egress = %v, want %v", l.egress, src)
	}
}

func TestLoop_WithProbe_NonNilSets(t *testing.T) {
	l := &Loop{}
	p := &Probe{}
	if got := l.WithProbe(p); got != l {
		t.Error("WithProbe did not return receiver")
	}
	if l.probe != p {
		t.Errorf("probe = %v, want %v", l.probe, p)
	}
}

// WithProbe(nil) is a documented no-op (per docstring — nil
// probe means "don't probe"). Pin the contract.
func TestLoop_WithProbe_NilNoop(t *testing.T) {
	l := &Loop{probe: &Probe{}}
	l.WithProbe(nil)
	if l.probe == nil {
		t.Error("WithProbe(nil): probe set to nil (should be no-op)")
	}
}

func TestLoop_WithPartitionCreate_NonNilSets(t *testing.T) {
	l := &Loop{}
	pc := func(_ context.Context) {}
	if got := l.WithPartitionCreate(pc); got != l {
		t.Error("WithPartitionCreate did not return receiver")
	}
	if l.partitionCreate == nil {
		t.Error("partitionCreate = nil, want non-nil")
	}
}

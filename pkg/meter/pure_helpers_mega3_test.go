// pure_helpers_mega3_test.go — Coverage Mega-PR #3 cluster E:
// fill pkg/meter coverage on the small, constructor-style pure
// helpers that don't need a store / DB / scheduler fixture.
//
// Targets (baseline 73.6% on the package at branch time):
//   - math.CPUHours (0%): microseconds → CPU-hours conversion.
//   - sampler.NewSamplerWithTail (0%): the issue #667 / ADR-078
//     constructor that wires the per-instance tail-seconds source.
//   - sampler.(*Sampler).tailSecondsFor (33.3%): the (int64, bool)
//     wrapper.
//   - loop.(*Loop).WithEgress / WithProbe / WithPartitionCreate
//     (0%): three chainable setters.
//
// Whitebox `package meter`.

package meter

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

func TestCPUHours_Mega3(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		cpuUsec int64
		want    float64
	}{
		{"zero", 0, 0},
		{"one hour in usec", int64(3.6e9), 1.0},
		{"half hour in usec", int64(1.8e9), 0.5},
		{"one minute in usec", int64(60_000_000), 60_000_000.0 / 3.6e9},
		{"one second in usec", int64(1_000_000), 1_000_000.0 / 3.6e9},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := CPUHours(c.cpuUsec)
			if diff := got - c.want; diff > 1e-9 || diff < -1e-9 {
				t.Errorf("CPUHours(%d) = %v, want %v", c.cpuUsec, got, c.want)
			}
		})
	}
}

func TestNewSamplerWithTail_Mega3(t *testing.T) {
	t.Parallel()
	now := func() time.Time { return time.Unix(0, 0) }
	tail := stubTailSourceMega3{}

	s := NewSamplerWithTail(nil, nil, nil, tail, now)
	if s == nil {
		t.Fatal("NewSamplerWithTail: got nil sampler")
	}
	if s.tail == nil {
		t.Error("NewSamplerWithTail: tail source not wired")
	}
	if s.now == nil {
		t.Error("NewSamplerWithTail: now not wired")
	}
	got, ok := s.tail.ReadAndResetTailSeconds("any")
	_ = got
	_ = ok
}

func TestNewSamplerWithTail_NilNowDefaultsToTimeNow_Mega3(t *testing.T) {
	t.Parallel()
	s := NewSamplerWithTail(nil, nil, nil, nil, nil)
	if s == nil {
		t.Fatal("NewSamplerWithTail: got nil sampler")
	}
	if s.now == nil {
		t.Error("NewSamplerWithTail(nil, nil, nil, nil, nil): now stayed nil")
	}
}

// stubTailSource is a minimal TailSecondsSource impl.
type stubTailSourceMega3 struct {
	seconds int64
	found   bool
	calls   int
}

func (s stubTailSourceMega3) ReadAndResetTailSeconds(_ string) (int64, bool) {
	s.calls++
	return s.seconds, s.found
}

func TestTailSecondsFor_NilSource_Mega3(t *testing.T) {
	t.Parallel()
	s := &Sampler{}
	v, ok := s.tailSecondsFor("inst-1")
	if v != 0 || ok {
		t.Errorf("nil-source: got (%d, %v), want (0, false)", v, ok)
	}
}

func TestTailSecondsFor_SourceReturnsNotFound_Mega3(t *testing.T) {
	t.Parallel()
	s := &Sampler{tail: stubTailSourceMega3{seconds: 999, found: false}}
	v, ok := s.tailSecondsFor("inst-2")
	if v != 0 || ok {
		t.Errorf("source-not-found: got (%d, %v), want (0, false)", v, ok)
	}
}

func TestTailSecondsFor_SourceReturnsValue_Mega3(t *testing.T) {
	t.Parallel()
	s := &Sampler{tail: stubTailSourceMega3{seconds: 42, found: true}}
	v, ok := s.tailSecondsFor("inst-3")
	if v != 42 || !ok {
		t.Errorf("source-found: got (%d, %v), want (42, true)", v, ok)
	}
}

// --- Loop setters --------------------------------------------------

func TestLoopWithEgress_Mega3(t *testing.T) {
	t.Parallel()
	l := &Loop{}
	got := l.WithEgress(nil)
	if got != l {
		t.Error("WithEgress must return receiver")
	}
}

func TestLoopWithProbe_Mega3(t *testing.T) {
	t.Parallel()
	l := &Loop{}
	l.WithProbe(nil)
	if l.probe != nil {
		t.Error("WithProbe(nil): probe should stay nil")
	}
	l = &Loop{}
	p := &Probe{}
	l.WithProbe(p)
	if l.probe != p {
		t.Errorf("WithProbe(p): l.probe = %p, want %p", l.probe, p)
	}
}

func TestLoopWithPartitionCreate_Mega3(t *testing.T) {
	t.Parallel()
	l := &Loop{}
	l.WithPartitionCreate(nil)
	if l.partitionCreate != nil {
		t.Error("WithPartitionCreate(nil): fn should stay nil")
	}
	l = &Loop{}
	called := false
	fn := func(_ context.Context) { called = true }
	l.WithPartitionCreate(fn)
	if l.partitionCreate == nil {
		t.Fatal("WithPartitionCreate(fn): fn not wired")
	}
	l.partitionCreate(context.Background())
	if !called {
		t.Error("WithPartitionCreate(fn): wired fn was not invoked")
	}
}

func TestNewLoop_NilSafetyDefaults_Mega3(t *testing.T) {
	t.Parallel()
	l := NewLoop(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, slog.Default(), nil, nil)
	if l == nil {
		t.Fatal("NewLoop: got nil loop")
	}
	if l.now == nil {
		t.Error("NewLoop(nil now): now default missing")
	}
	if l.log == nil {
		t.Error("NewLoop(nil log): log default missing")
	}
	if l.ops == nil {
		t.Error("NewLoop(nil ops): ops default missing")
	}
	if l.residency == nil {
		t.Error("NewLoop(nil residency): residency default missing")
	}
	if l.mailer == nil {
		t.Error("NewLoop(nil mailer): mailer default missing")
	}
	if l.lastTick == nil {
		t.Error("NewLoop: lastTick map missing")
	}
}

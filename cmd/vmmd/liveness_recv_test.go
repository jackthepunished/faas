//go:build linux

// Tests for the liveness-probe counter + cooldown classification
// (issue #554 / ADR-078). These tests pin the AC #2 surface
// (flaky app does NOT oscillate) directly:
//
//   - A 200/200/500/200/200/500 sequence resets the
//     consecutive-failure counter on the first 2xx, so the
//     destroy never fires.
//   - A back-to-back 500/500/500/500 sequence reaches the
//     ConsecutiveFailures threshold (3) on the third 500 and
//     triggers the relay exactly once.
//
// We exercise the runOne classification path via the probeFn
// seam (cmd/vmmd/liveness_recv.go::livenessProbeLoop.probeFn),
// bypassing the real AF_VSOCK dial so the test runs on any
// Linux dev box without KVM.
package main

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/fcvm"
)

// recordingSink counts the calls to Manager.ReportLivenessFailed
// so the test can assert "exactly one relay fire" without
// wiring a real schedd engine.
type recordingSink struct {
	mu    sync.Mutex
	calls []sinkCall
}

type sinkCall struct {
	instance string
	reason   string
}

func (s *recordingSink) Record(_ context.Context, instance, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, sinkCall{instance, reason})
}

func (s *recordingSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// newTestLoop builds a *livenessProbeLoop wired to a real
// *fcvm.Manager with the liveness-sink relay pointed at a
// recordingSink. Returns the loop + the sink so the test can
// assert on the side effects.
func newTestLoop(t *testing.T, instanceID string, consec int) (*livenessProbeLoop, *recordingSink, *fcvm.Manager) {
	t.Helper()
	sink := &recordingSink{}
	mgr := fcvm.NewManager(nil, nil, fcvm.Paths{}, "1.10.0", slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	// Attach the sink via the WithLivenessSink helper. The
	// post-F1 Manager signature returns *Manager (chainable)
	// and accepts the named LivenessFailedSink parameter type —
	// the test interface must declare the *named* parameter type
	// (not a bare func(...) form) for Go's interface satisfaction
	// to allow the type assertion. Passing a bare `func(...)`
	// here fails at runtime with "does not expose WithLivenessSink
	// — test seam missing" even though the underlying call
	// signature is identical — Go's interface assignability
	// checks parameter-type identity for named types.
	type sinkSetter interface {
		WithLivenessSink(fcvm.LivenessFailedSink) *fcvm.Manager
	}
	if ss, ok := any(mgr).(sinkSetter); ok {
		// Explicit conversion: sink.Record is a method value with
		// the bare func type `func(ctx context.Context, instance,
		// reason string)`. Production declares the parameter as
		// the named `LivenessFailedSink` alias. Identical shape,
		// distinct types at the type-system layer — must convert
		// to satisfy the interface call.
		ss.WithLivenessSink(fcvm.LivenessFailedSink(sink.Record))
	} else {
		t.Fatalf("fcvm.Manager does not expose WithLivenessSink — test seam missing")
	}
	loop := &livenessProbeLoop{
		instance: instanceID,
		cfg: livenessProbeConfig{
			Path:                "/healthz",
			PeriodSeconds:       5,
			ConsecutiveFailures: consec,
			CooldownSeconds:     60,
		},
		cid: 0, // unused in tests (probeFn bypasses dial)
		mgr: mgr,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return loop, sink, mgr
}

// TestLivenessRecv_CounterSurvivesIntermittentSuccess is AC #2:
// 200/200/500/200/200/500 does NOT fire DestroyForLivenessFailure.
// The first 200 reset the counter; subsequent failures
// re-increment from 0; ConsecutiveFailures=3 means we need
// 3 in a row WITHOUT a 200 in between.
func TestLivenessRecv_CounterSurvivesIntermittentSuccess(t *testing.T) {
	loop, sink, _ := newTestLoop(t, "inst-1", 3)
	// Stubbed probe: emits the outcome sequence 200/200/500/
	// 200/200/500 — index-driven, the loop calls runOne in a
	// loop so we advance per call.
	outcomes := []string{
		livenessOutcomeOK,
		livenessOutcomeOK,
		livenessOutcomeNon200,
		livenessOutcomeOK,
		livenessOutcomeOK,
		livenessOutcomeNon200,
	}
	loop.probeFn = func(_ context.Context, _ int) string {
		if len(outcomes) == 0 {
			return livenessOutcomeOK
		}
		out := outcomes[0]
		outcomes = outcomes[1:]
		return out
	}
	for i := 0; i < 6; i++ {
		loop.runOne(context.Background(), 2000)
	}
	if sink.count() != 0 {
		t.Errorf("sink.count = %d, want 0 (AC #2: intermittent 200 must reset counter)", sink.count())
	}
}

// TestLivenessRecv_ThreeConsecFires is the success path: 3 in a
// row of non_200 → relay fires exactly once.
func TestLivenessRecv_ThreeConsecFires(t *testing.T) {
	loop, sink, _ := newTestLoop(t, "inst-2", 3)
	loop.probeFn = func(_ context.Context, _ int) string {
		return livenessOutcomeNon200
	}
	for i := 0; i < 3; i++ {
		loop.runOne(context.Background(), 2000)
	}
	if sink.count() != 1 {
		t.Errorf("sink.count = %d, want 1 (3 consecutive non_200 must fire relay)", sink.count())
	}
	// 4th call after the relay fires — the loop's runOne has
	// already returned. The relay is expected to be invoked
	// AT MOST once per runOne cycle; a 4th runOne after a
	// successful park exits the production goroutine, but a
	// unit test of runOne in isolation doesn't have that exit
	// — the counter is reset to 0 implicitly by the relay's
	// exit path. We assert the test-side count remains 1.
}

// TestLivenessRecv_TimeoutCountedClassifies is the classification
// pin: timeout outcome increments the counter (same code path
// as non_200) and lands a "liveness_timeout" reason string on
// the relay.
func TestLivenessRecv_TimeoutCountedClassifies(t *testing.T) {
	loop, sink, _ := newTestLoop(t, "inst-3", 2)
	loop.probeFn = func(_ context.Context, _ int) string {
		return livenessOutcomeTimeout
	}
	loop.runOne(context.Background(), 2000)
	loop.runOne(context.Background(), 2000)
	if sink.count() != 1 {
		t.Errorf("sink.count = %d, want 1 (2 consecutive timeouts must fire)", sink.count())
	}
}

// TestLivenessRecv_ConnRefusedCounted is the cold-boot signature:
// the guest-init listener isn't up yet on the first poll. The
// counter increments; the CooldownSeconds gate in the schedd
// side protects against noise.
func TestLivenessRecv_ConnRefusedCounted(t *testing.T) {
	loop, sink, _ := newTestLoop(t, "inst-4", 2)
	loop.probeFn = func(_ context.Context, _ int) string {
		return livenessOutcomeConnRefused
	}
	loop.runOne(context.Background(), 2000)
	loop.runOne(context.Background(), 2000)
	if sink.count() != 1 {
		t.Errorf("sink.count = %d, want 1 (conn_refused classifies same as non_200)", sink.count())
	}
}

// errDiscard removed (F9): the WithLivenessSink path lives in
// pkg/fcvm and is exercised by the manager_test there; this
// file no longer needs a sentinel.

// keep-time removed (F9): the test no longer references time.Time
// directly (the runOne signature is just (ctx, timeoutMs)); the
// liveness-window test at pkg/sched/liveness_window_test.go
// owns the time-driven paths.

// TestLivenessRecv_CooldownGateShortCircuits (issue #554 closure /
// ADR-078) pins the cooldown gate at runOne: a probe failure that
// falls inside cfg.CooldownSeconds of the previous
// LastLivenessDestroyAt stamp must NOT increment the counter and
// must NOT fire the relay. The customer-visible scenario is "I
// just had a wedged VM torn down, the cold-boot replacement is
// still warming up — don't tear it down too". Pre-#554 this
// scenario was unreliable: the first 3 probe failures (typically
// conn_refused during the guest-init listener bring-up) would
// fire DestroyForLivenessFailure on a perfectly healthy cold-boot
// instance. The gate bypasses cleanly when CooldownSeconds == 0
// (Free plan / legacy callers) or LastLivenessDestroyAt is zero
// (no prior destroy recorded).
func TestLivenessRecv_CooldownGateShortCircuits(t *testing.T) {
	loop, sink, mgr := newTestLoop(t, "inst-cd", 3)
	// Register the instance so Manager.LastLivenessDestroyAt
	// returns a non-zero value.
	mgr.RegisterInstanceForTest("inst-cd")
	// Stamp a destroy 5 seconds in the past. cfg.CooldownSeconds=60
	// (set by newTestLoop), so a probe now falls well within the
	// window.
	mgr.SetLastLivenessDestroyAtForTest("inst-cd", time.Now().Add(-5*time.Second))

	// All three probes are non_200, but the gate must short-circuit.
	loop.probeFn = func(_ context.Context, _ int) string {
		return livenessOutcomeNon200
	}
	for i := 0; i < 3; i++ {
		loop.runOne(context.Background(), 2000)
	}
	if sink.count() != 0 {
		t.Errorf("sink.count = %d, want 0 (cooldown gate must short-circuit fires within 60s)", sink.count())
	}
}

// TestLivenessRecv_CooldownGateExpires confirms the bypass: a
// destroy that's older than cfg.CooldownSeconds does NOT
// short-circuit. Without the bypass a customer's deployment
// would be parked forever once the first destroy happened.
func TestLivenessRecv_CooldownGateExpires(t *testing.T) {
	loop, sink, mgr := newTestLoop(t, "inst-cd-2", 3)
	mgr.RegisterInstanceForTest("inst-cd-2")
	// Stamp a destroy 120 seconds in the past. CooldownSeconds=60,
	// so we're well outside the window.
	mgr.SetLastLivenessDestroyAtForTest("inst-cd-2", time.Now().Add(-120*time.Second))

	loop.probeFn = func(_ context.Context, _ int) string {
		return livenessOutcomeNon200
	}
	for i := 0; i < 3; i++ {
		loop.runOne(context.Background(), 2000)
	}
	if sink.count() != 1 {
		t.Errorf("sink.count = %d, want 1 (cooldown expired, 3 consec must fire)", sink.count())
	}
}

// TestLivenessRecv_CooldownGateZeroCooldownBypasses confirms the
// legacy / Free-plan path: CooldownSeconds=0 means "no cooldown
// gate" — pre-#554 behaviour. The destroy stamp is ignored.
func TestLivenessRecv_CooldownGateZeroCooldownBypasses(t *testing.T) {
	loop, sink, mgr := newTestLoop(t, "inst-cd-3", 2)
	loop.cfg.CooldownSeconds = 0 // legacy / Free
	mgr.RegisterInstanceForTest("inst-cd-3")
	mgr.SetLastLivenessDestroyAtForTest("inst-cd-3", time.Now().Add(-1*time.Second))

	loop.probeFn = func(_ context.Context, _ int) string {
		return livenessOutcomeNon200
	}
	for i := 0; i < 2; i++ {
		loop.runOne(context.Background(), 2000)
	}
	if sink.count() != 1 {
		t.Errorf("sink.count = %d, want 1 (CooldownSeconds=0 bypasses the gate)", sink.count())
	}
}

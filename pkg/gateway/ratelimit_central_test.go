// ratelimit_central_test.go — pins the central-mode boundary-case
// consult (ADR-104 amendment 5, issue #881 Phase 4 C3).
//
// The fast-path-cache pattern: in-process bucket serves Peek
// (the response-header writer); PG consulted only when local
// would reject. A scratch backend counts every call so the
// tests can assert:
//   - Noop backend: zero PG round-trips (the default production
//     posture for single-box dev).
//   - Real backend: PG consulted exactly on the local-would-
//     reject boundary case — never on admits, never on Peek.
//   - Degraded: PG error path returns false (in-process reject
//     preserved) and does not panic.
package gateway

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
)

// fakeCentral is a CentralBackend test double. Round-trip counters
// let the tests assert "did the limiter actually call the backend".
type fakeCentral struct {
	consumeCalls atomic.Int64
	peekCalls    atomic.Int64
	invalCalls   atomic.Int64

	// peekResult returns the (remaining, err) the backend yields
	// on every PeekToken. Tests flip it mid-run to model central
	// refill. Default (0, nil) means "central says reject" — the
	// boundary case lands here for the noop-equivalence tests.
	peekResult func() (int, error)
}

func newFakeCentral() *fakeCentral {
	return &fakeCentral{}
}

func (f *fakeCentral) ConsumeToken(_ context.Context, _, _, _ string) (int, bool, error) {
	f.consumeCalls.Add(1)
	return 0, true, nil
}

func (f *fakeCentral) PeekToken(_ context.Context, _, _, _ string) (int, error) {
	f.peekCalls.Add(1)
	if f.peekResult != nil {
		return f.peekResult()
	}
	return 0, nil
}

func (f *fakeCentral) Invalidate(_, _, _ string) {
	f.invalCalls.Add(1)
}

func TestLimiter_NoopBackend_NeverConsultsCentral(t *testing.T) {
	// Production default — Limiter built with NewLimiter has a
	// noopCentralBackend that never reaches Postgres. The
	// isNoopBackend shortcut must short-circuit the boundary-
	// case consult so a single-box dev cluster never pays a
	// goroutine-y cost on the central path.
	l := NewLimiter()
	for i := 0; i < 100; i++ {
		if !l.Allow(context.Background(), "appid", api.PlanHobby) {
			t.Fatalf("hobby plan rejected admit #%d (back-compat: noop backend never blocks)", i)
		}
	}
	// Pin: the default backend is the noop.
	if !l.isNoopBackend() {
		t.Error("isNoopBackend: false on NewLimiter (default must be noop)")
	}
	// Pin: a Limiter built with NewLimiterWithCentral(nil)
	// also defaults to the noop — nil backend MUST NOT cause a
	// nil-pointer deref at the boundary case.
	l2 := NewLimiterWithCentral(nil)
	if !l2.isNoopBackend() {
		t.Error("isNoopBackend: false on NewLimiterWithCentral(nil) (nil backend must fall back to noop)")
	}
}

func TestLimiter_RealBackend_BoundaryConsultOnly(t *testing.T) {
	// The boundary case: in-process bucket rejects, central
	// consults, admit returns true iff central has tokens.
	// Tests:
	//   - 100 admits: zero central round-trips (in-process serves
	//     the burst).
	//   - Drain the in-process bucket.
	//   - Next call → in-process rejects; central is consulted
	//     once. Central returns (5, nil) → admit. Central round-
	//     trip count is exactly 1.
	fake := newFakeCentral()
	// Frozen clock: prevent in-process refill between iterations
	// so the bucket is reliably drained after the burst. Without
	// this, Hobby's 20 rps + wall-clock latency refills a few
	// tokens during the loop and the boundary-case path never
	// fires.
	frozen := time.Unix(1_700_000_000, 0)
	l := NewLimiterWithCentralAndClock(fake, func() time.Time { return frozen })

	// Hobby-shaped bucket: 20 rps, 100 burst. The central key
	// opt-in is what activates the boundary-case consult; Allow /
	// AllowAccount stay back-compat with empty central key.
	const rps, burst = 20.0, 100.0
	const centralKey = "app:00000000-0000-0000-0000-000000000001:hobby"

	// First 100 admits fit in burst; the frozen clock prevents
	// any refill between them. Zero central round-trips.
	for i := 0; i < 100; i++ {
		if !l.AllowWithCentralParams(context.Background(), "appid", rps, burst, centralKey) {
			t.Fatalf("hobby admit #%d rejected in burst (in-process should serve)", i)
		}
	}
	if got := fake.peekCalls.Load(); got != 0 {
		t.Errorf("peek calls during burst = %d, want 0 (in-process serves admits)", got)
	}

	// Bucket drained. The next call must consult central exactly
	// once. Central returns (5, nil) → admit.
	fake.peekResult = func() (int, error) { return 5, nil }
	if !l.AllowWithCentralParams(context.Background(), "appid", rps, burst, centralKey) {
		t.Errorf("hobby admit on drained bucket rejected; central said admit (remaining=5)")
	}
	if got := fake.peekCalls.Load(); got != 1 {
		t.Errorf("peek calls after drain = %d, want 1 (boundary-case consult)", got)
	}

	// Central still rejects → reject.
	fake.peekResult = func() (int, error) { return 0, nil }
	if l.AllowWithCentralParams(context.Background(), "appid", rps, burst, centralKey) {
		t.Error("hobby admit accepted despite central reject (degraded posture must preserve local reject)")
	}
	if got := fake.peekCalls.Load(); got != 2 {
		t.Errorf("peek calls after second drain = %d, want 2", got)
	}
}

func TestLimiter_RealBackend_PGErrorDegradesSoft(t *testing.T) {
	// Postgres unreachable: PeekToken returns error → limiter
	// returns false (preserves local reject decision) without
	// panicking. A separate ratelimit_degraded audit row is the
	// operator signal (the audit emitter lives in the daemon,
	// not the limiter — out of scope here).
	fake := newFakeCentral()
	fake.peekResult = func() (int, error) { return 0, errors.New("postgres down") }
	// Frozen clock so the burst test doesn't refill between
	// iterations — we need a deterministic drain.
	frozen := time.Unix(1_700_000_000, 0)
	l := NewLimiterWithCentralAndClock(fake, func() time.Time { return frozen })

	// Scale-shaped bucket: 1500 rps, 3000 burst (per
	// pkg/api/limits.go Scale plan). Drain under frozen clock,
	// then a single call must reject (degraded posture
	// preserves the local reject).
	const rps, burst = 1500.0, 3000.0
	const centralKey = "app:00000000-0000-0000-0000-000000000002:scale"
	for i := 0; i < 3000; i++ {
		l.AllowWithCentralParams(context.Background(), "appid", rps, burst, centralKey)
	}
	if l.AllowWithCentralParams(context.Background(), "appid", rps, burst, centralKey) {
		t.Error("scale admit accepted despite PG error (degraded posture must preserve local reject)")
	}
	if got := fake.peekCalls.Load(); got == 0 {
		t.Error("peek calls during PG-error test = 0, want >= 1 (degraded path MUST consult central to surface the failure mode)")
	}
}

func TestSplitCentralKey_AcceptsValidTriples(t *testing.T) {
	cases := []struct {
		in        string
		scope     string
		subjectID string
		plan      string
	}{
		{"app:00000000-0000-0000-0000-000000000001:hobby", "app", "00000000-0000-0000-0000-000000000001", "hobby"},
		{"account:00000000-0000-0000-0000-000000000002:scale", "account", "00000000-0000-0000-0000-000000000002", "scale"},
		{"rule:00000000-0000-0000-0000-000000000003:pro", "rule", "00000000-0000-0000-0000-000000000003", "pro"},
	}
	for _, c := range cases {
		s, sid, p, ok := splitCentralKey(c.in)
		if !ok {
			t.Errorf("splitCentralKey(%q): !ok", c.in)
			continue
		}
		if s != c.scope || sid != c.subjectID || p != c.plan {
			t.Errorf("splitCentralKey(%q): got (%q, %q, %q), want (%q, %q, %q)", c.in, s, sid, p, c.scope, c.subjectID, c.plan)
		}
	}
}

func TestSplitCentralKey_RejectsMalformed(t *testing.T) {
	bad := []string{
		"",          // empty
		"app",       // one segment
		"app:hobby", // two segments
		":00000000-0000-0000-0000-000000000001:hobby", // empty scope
		"app::hobby", // empty subject_id
		"app:00000000-0000-0000-0000-000000000001:",        // empty plan
		"app:00000000-0000-0000-0000-000000000001:unknown", // unknown plan
		"route:00000000-0000-0000-0000-000000000001:hobby", // unknown scope (typo: 'route' is Phase 3 *action* vocab)
	}
	for _, c := range bad {
		if _, _, _, ok := splitCentralKey(c); ok {
			t.Errorf("splitCentralKey(%q): want !ok", c)
		}
	}
}

func TestSplitCentralKey_RejectsUnknownPlan(t *testing.T) {
	// Defence-in-depth: even if a caller passes a closed-vocab
	// scope, an unknown plan must be rejected so a future
	// "enterprise" plan addition isn't silently bypassed.
	for _, plan := range []string{"", "starter", "business", "ENTERPRISE"} {
		key := "app:00000000-0000-0000-0000-000000000001:" + strings.ToLower(plan)
		if _, _, _, ok := splitCentralKey(key); ok {
			t.Errorf("splitCentralKey(%q): accepted unknown plan %q", key, plan)
		}
	}
}

func TestLimiter_AllowWithCentralParams_NoCentralKey_NoConsult(t *testing.T) {
	// Empty centralKey: the call site is back-compat with
	// AllowWithParams (per-app or per-account). The
	// boundary-case consult MUST NOT fire — the production
	// per-app + per-account call sites pass empty until C3.5.
	fake := newFakeCentral()
	l := NewLimiterWithCentral(fake)
	for i := 0; i < 50; i++ {
		l.AllowWithCentralParams(context.Background(), "appid", 50, 100, "")
	}
	if got := fake.peekCalls.Load(); got != 0 {
		t.Errorf("peek calls with empty centralKey = %d, want 0 (back-compat)", got)
	}
}

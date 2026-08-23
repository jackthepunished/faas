// pure_helpers_test.go — fill pkg/db/db.go + notify.go coverage
// of the helpers that don't require spinning up Postgres, and
// cover the ones that do via pgtest.Open(t) so the test ships
// on the same `unit-tests-pg-*` lanes as the rest of the file.
//
// Targets:
//   - WithBudget (the no-op / overhead-reservation branches)
//   - PoolNotifier.Notify (the round-trip through pg_notify)
//   - NotifyChannels round-trip invariant (channel constants)
//   - BuildQueuedPayload JSON shape
//
// Whitebox `package db`.
package db

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/db/pgtest"
	"github.com/onebox-faas/faas/pkg/reqbudget"
)

// --- WithBudget -------------------------------------------------

// WithBudget returns parent unchanged when no Budget is attached.
// The helper is the identity no-op for CLI / background paths.
func TestWithBudget_NoBudgetIsIdentity(t *testing.T) {
	parent := context.Background()
	got := WithBudget(parent)
	if got != parent {
		t.Error("no budget: got != parent, want identity")
	}
}

// WithBudget, when the parent carries a Budget, returns a ctx
// with a derived deadline (remaining - DefaultOverheadDB). The
// returned ctx preserves the parent Budget through context.WithTimeout
// value inheritance (the canonical consumer — pgstore — uses this
// to attach a tighter deadline to the DB hop without losing the
// original Budget for downstream SQL instrumentation).
func TestWithBudget_AttachesReservation(t *testing.T) {
	b := reqbudget.Budget{
		Total:   200 * time.Millisecond,
		Started: time.Now(),
	}
	ctx := reqbudget.NewContext(context.Background(), b)
	out := WithBudget(ctx)
	if out == nil {
		t.Fatal("nil ctx")
	}
	// Parent Budget is still attached (context.WithTimeout
	// preserves values).
	got, ok := reqbudget.FromContext(out)
	if !ok {
		t.Fatal("after WithBudget: FromContext = false, want true")
	}
	if got.Total != b.Total {
		t.Errorf("Total = %v, want %v", got.Total, b.Total)
	}
	// A deadline must be installed (the load-bearing side effect
	// — pgstore reads ctx.Deadline() to bound its SQL hops).
	dl, hasDL := out.Deadline()
	if !hasDL {
		t.Fatal("after WithBudget: no deadline, want deadline derived from Budget")
	}
	if dl.IsZero() {
		t.Error("deadline is zero time")
	}
}

// WithBudget chained twice — each call installs a tighter
// deadline on the returned ctx. Both deadlines must be non-zero
// (the second is monotonically closer to now).
func TestWithBudget_ChainAddsReservations(t *testing.T) {
	b := reqbudget.Budget{Total: time.Second, Started: time.Now()}
	ctx := reqbudget.NewContext(context.Background(), b)
	dl1, has1 := WithBudget(ctx).Deadline()
	if !has1 {
		t.Fatal("first WithBudget: no deadline")
	}
	dl2, has2 := WithBudget(WithBudget(ctx)).Deadline()
	if !has2 {
		t.Fatal("second WithBudget: no deadline")
	}
	if dl2.After(dl1) {
		t.Errorf("chained deadline %v should be ≤ first %v", dl2, dl1)
	}
}

// WithBudget on a parent with no budget but with a deadline
// returns parent (the deadline is preserved verbatim).
func TestWithBudget_DeadlineParentPassesThrough(t *testing.T) {
	parent, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Second))
	defer cancel()
	got := WithBudget(parent)
	if _, ok := got.Deadline(); !ok {
		t.Error("deadline parent: lost deadline after WithBudget")
	}
}

// --- PoolNotifier.Notify ----------------------------------------

// PoolNotifier.Notify round-trips through pg_notify via pgtest.
// We don't observe the LISTEN side here (that's the LISTEN tests);
// this asserts the producer side doesn't error.
func TestPoolNotifier_Notify_RoundTrip(t *testing.T) {
	pool := pgtest.Open(t)
	defer pool.Close()
	pn := PoolNotifier{Pool: pool}
	if err := pn.Notify(context.Background(), "extra_helpers_ch", `{"k":"v"}`); err != nil {
		t.Fatalf("Notify: %v", err)
	}
}

// Notify on a closed-pool surfaces an error wrapping the
// underlying pg_notify failure.
func TestPoolNotifier_Notify_ClosedPoolErrors(t *testing.T) {
	pool := pgtest.Open(t)
	pool.Close() // close immediately so subsequent Exec errors
	if err := Notify(context.Background(), pool, "x", "y"); err == nil {
		t.Error("closed pool: got nil err, want error")
	}
}

// --- BuildQueuedPayload JSON round-trip ------------------------

// The producer side emits this struct; consumers decode it back.
// Round-trip catches field-tag drift.
func TestBuildQueuedPayload_RoundTrip(t *testing.T) {
	in := BuildQueuedPayload{
		BuildID:      "build-1",
		DeploymentID: "dep-1",
		AppID:        "app-1",
		Kind:         "tarball",
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out BuildQueuedPayload
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Errorf("got %+v, want %+v", out, in)
	}
}

// Empty payload still round-trips (the listener side accepts
// `{}` and treats it as a heartbeat with no resource IDs).
func TestBuildQueuedPayload_EmptyRoundTrip(t *testing.T) {
	in := BuildQueuedPayload{}
	b, _ := json.Marshal(in)
	var out BuildQueuedPayload
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Errorf("got %+v, want %+v", out, in)
	}
}
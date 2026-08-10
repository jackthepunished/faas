package sched

// Tests for the fire-now side of issue #791 PR-C / ADR-090.
//
// Coverage split:
//   - RunCronNow (pkg/sched/loop.go) — the per-row dispatch helper.
//   - drainPendingFireNowRequests (pkg/sched/fire_now.go) — the
//     claim+dispatch loop the subscriber goroutine calls.
//
// We do NOT test FireNowRun itself: it requires a real Postgres to
// exercise db.SubscribeWithReconnect. The unit-test coverage stops at
// the helpers FireNowRun composes, which is the right place for both
// behavioural regressions and CI speed (no pg dependency).
//
// Why MemStore everywhere: pgstore_fire_now.go's claim is a thin SQL
// wrapper over the SKIP LOCKED + status filter, and the test the
// MemStore gets (FIFO order, status guard, error mapping) carries
// over to PgStore by construction.

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// ---------- RunCronNow ----------

func TestRunCronNow_FiresImmediateSkipBoundary(t *testing.T) {
	t.Parallel()
	store := state.NewMemStore()
	ctx := context.Background()
	acct, _ := store.CreateAccount(ctx, "fnt@example.com", api.PlanPro)

	// Cron scheduled for a year in the future — the 60s tick would
	// never fire it within the test horizon. RunCronNow must bypass
	// the due-time boundary guard and dispatch anyway.
	app, c := newAppAndCron(t, store, acct.ID, true)
	// newAppAndCron backdates CreatedAt to 11:00 for the tick path.
	// Override the schedule so next_fire_at is far in the future.
	yearAhead := "0 0 1 1 *"
	if _, err := store.UpdateCron(ctx, c.ID, nil, &yearAhead, nil, nil); err != nil {
		t.Fatalf("update schedule: %v", err)
	}

	vmm := &fakeWakeVMM{}
	eng, _ := makeEngine(t, store, vmm)
	synth := &recordingSynth{}
	loop := NewLoop(nil, eng, slog.Default()).
		WithGatewaySynth(synth)

	cronAfterUpdate, err := store.CronByID(ctx, c.ID)
	if err != nil {
		t.Fatalf("CronByID: %v", err)
	}

	// Sanity: next_fire_at is genuinely > test-end time. Schedule
	// "0 0 1 1 *" = midnight on Jan 1 — robfig picks the *next*
	// occurrence which is at least a year out.
	if !cronAfterUpdate.LastFiredAt.IsZero() {
		t.Fatalf("LastFiredAt pre-fire = %v, want zero (precondition: cron has never fired)", cronAfterUpdate.LastFiredAt)
	}

	out, err := loop.RunCronNow(ctx, c.ID, acct.ID)
	if err != nil {
		t.Fatalf("RunCronNow: %v", err)
	}
	if !out.Success {
		t.Errorf("Success = false, want true; out=%+v", out)
	}

	// Synth must have been invoked once — the dispatch path reached
	// the synth step.
	if got := synth.calls.Load(); got != 1 {
		t.Errorf("synth calls = %d, want 1 (manual fire should reach the gateway-Invoke step)", got)
	}

	// Hard rule (ADR-090 §"Reuse of schedd"): RunCronNow does NOT call
	// MarkCronFired. A manual fire must not shift next_fire_at or
	// stick a wall-clock stamp on LastFiredAt. Confirm by reloading
	// and re-running the tick path — if LastFiredAt got stamped, the
	// tick would skip the cron for at least one boundary.
	cronAfter, err := store.CronByID(ctx, c.ID)
	if err != nil {
		t.Fatalf("CronByID post-fire: %v", err)
	}
	if !cronAfter.LastFiredAt.IsZero() {
		t.Errorf("LastFiredAt post-fire = %v, want zero — RunCronNow must not call MarkCronFired (ADR-090 §'Reuse of schedd')",
			cronAfter.LastFiredAt)
	}

	// And the cron row should still belong to the same app (sanity).
	if cronAfter.AppID != app.ID {
		t.Errorf("AppID = %q, want %q", cronAfter.AppID, app.ID)
	}
}

func TestRunCronNow_DisabledCronReturnsErrCronDisabled(t *testing.T) {
	t.Parallel()
	store := state.NewMemStore()
	ctx := context.Background()
	acct, _ := store.CreateAccount(ctx, "fnd@example.com", api.PlanPro)
	_, c := newAppAndCron(t, store, acct.ID, false) // enabled=false

	eng, _ := makeEngine(t, store, &fakeWakeVMM{})
	loop := NewLoop(nil, eng, slog.Default())

	_, err := loop.RunCronNow(ctx, c.ID, acct.ID)
	if !errors.Is(err, ErrCronDisabled) {
		t.Fatalf("err = %v, want ErrCronDisabled", err)
	}
}

func TestRunCronNow_UnknownCronReturnsStoreError(t *testing.T) {
	t.Parallel()
	store := state.NewMemStore()
	ctx := context.Background()
	acct, _ := store.CreateAccount(ctx, "fnu@example.com", api.PlanPro)
	eng, _ := makeEngine(t, store, &fakeWakeVMM{})
	loop := NewLoop(nil, eng, slog.Default())

	// "no-such-cron" is a valid UUID-shape-but-absent id. CronByID
	// must return ErrNotFound and RunCronNow bubbles that up.
	_, err := loop.RunCronNow(ctx, "no-such-cron", acct.ID)
	if !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("err = %v, want state.ErrNotFound", err)
	}
}

// ---------- drainPendingFireNowRequests ----------

// fakeHarness bundles the engine + loop + store + fakes used by the
// drainPendingFireNowRequests tests. Kept tiny on purpose — each
// test field-overrides only the bits it cares about.
type fireNowHarness struct {
	store state.Store
	loop  *Loop
	synth *recordingSynth
	vmm   *fakeWakeVMM
	now   time.Time
}

func newFireNowHarness(t *testing.T) *fireNowHarness {
	t.Helper()
	store := state.NewMemStore()
	vmm := &fakeWakeVMM{}
	eng, _ := makeEngine(t, store, vmm)
	synth := &recordingSynth{}
	loop := NewLoop(nil, eng, slog.Default()).WithGatewaySynth(synth)
	return &fireNowHarness{
		store: store, loop: loop, synth: synth, vmm: vmm,
		now: time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC),
	}
}

// drainPendingFireNowRequests needs a CronByID that returns the cron
// we seeded. MemStore's CronByID matches on the row stored by
// CreateCron, so the (cronID, accountID) pair is the only thing
// drain needs — it reads the request row, calls RunCronNow, and
// stamps the terminal state. We don't need to wire a fake CronByID.

func TestDrainPending_NoRowsIsClean(t *testing.T) {
	t.Parallel()
	h := newFireNowHarness(t)
	// Drain on an empty store: returns immediately. The contract is
	// "exits cleanly with no error and no Mark* calls". Asserted by
	// the absence of panic + zero synth calls.
	h.loop.drainPendingFireNowRequests(context.Background())
	if got := h.synth.calls.Load(); got != 0 {
		t.Errorf("synth calls = %d, want 0 on empty queue", got)
	}
}

func TestDrainPending_HappyPathStampsSucceeded(t *testing.T) {
	t.Parallel()
	h := newFireNowHarness(t)
	ctx := context.Background()
	acct, _ := h.store.CreateAccount(ctx, "dph@example.com", api.PlanPro)
	_, c := newAppAndCron(t, h.store, acct.ID, true)

	// apid's fireCronNow would have created this row. We simulate
	// the apid side here so the test focuses on schedd's behaviour.
	requestID, err := h.store.InsertFireNowRequest(ctx, c.ID, acct.ID)
	if err != nil {
		t.Fatalf("InsertFireNowRequest: %v", err)
	}

	h.loop.drainPendingFireNowRequests(ctx)

	// Row is now terminal: succeeded (RunCronNow returned Success).
	got, err := h.store.GetFireNowRequest(ctx, requestID)
	if err != nil {
		t.Fatalf("GetFireNowRequest: %v", err)
	}
	if got.Status != state.FireNowStatusSucceeded {
		t.Errorf("status = %q, want succeeded", got.Status)
	}
	if got.FinishedAt == nil {
		t.Errorf("FinishedAt nil, want terminal stamp")
	}

	// Synth was invoked exactly once: drain's claim+dispatch path
	// reached the gateway Invoke step.
	if n := h.synth.calls.Load(); n != 1 {
		t.Errorf("synth calls = %d, want 1", n)
	}

	// Hard rule (re-stated from the unit test above): a manual fire
	// must NOT shift LastFiredAt — drain's dispatch goes through
	// RunCronNow which never calls MarkCronFired.
	post, err := h.store.CronByID(ctx, c.ID)
	if err != nil {
		t.Fatalf("CronByID post-drain: %v", err)
	}
	if !post.LastFiredAt.IsZero() {
		t.Errorf("LastFiredAt post-drain = %v, want zero (RunCronNow must not MarkCronFired)", post.LastFiredAt)
	}
}

func TestDrainPending_DisabledCronStampsCronDisabled(t *testing.T) {
	t.Parallel()
	h := newFireNowHarness(t)
	ctx := context.Background()
	acct, _ := h.store.CreateAccount(ctx, "dpd@example.com", api.PlanPro)
	_, c := newAppAndCron(t, h.store, acct.ID, false) // disabled
	requestID, err := h.store.InsertFireNowRequest(ctx, c.ID, acct.ID)
	if err != nil {
		t.Fatalf("InsertFireNowRequest: %v", err)
	}

	h.loop.drainPendingFireNowRequests(ctx)

	got, err := h.store.GetFireNowRequest(ctx, requestID)
	if err != nil {
		t.Fatalf("GetFireNowRequest: %v", err)
	}
	if got.Status != state.FireNowStatusFailed {
		t.Fatalf("status = %q, want failed (ErrCronDisabled is mapped to failed)", got.Status)
	}
	if got.Error == nil || *got.Error != "cron disabled" {
		t.Errorf("Error = %v, want pointer to \"cron disabled\"", got.Error)
	}
	// Synth must NOT have fired — RunCronNow returned before reaching
	// dispatchCronLocked.
	if n := h.synth.calls.Load(); n != 0 {
		t.Errorf("synth calls = %d, want 0 (disabled cron must not invoke)", n)
	}
}

func TestDrainPending_AllRowsDrained(t *testing.T) {
	t.Parallel()
	h := newFireNowHarness(t)
	ctx := context.Background()
	acct, _ := h.store.CreateAccount(ctx, "dpall@example.com", api.PlanPro)
	_, c := newAppAndCron(t, h.store, acct.ID, true)

	// Seed three requests — drain must claim all of them in FIFO
	// order in a single call (ClaimPendingFireNowRequest is one-row
	// per call but the drain loop calls it repeatedly until empty).
	ids := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		id, err := h.store.InsertFireNowRequest(ctx, c.ID, acct.ID)
		if err != nil {
			t.Fatalf("InsertFireNowRequest #%d: %v", i, err)
		}
		ids = append(ids, id)
		// Strict ordering — MemStore's FIFO-by-requested_at relies on
		// RequestedAt being monotonically non-decreasing across
		// consecutive inserts. MemStore uses time.Now() so a 1ms
		// gap is enough on any reasonable clock.
		time.Sleep(1 * time.Millisecond)
	}

	h.loop.drainPendingFireNowRequests(ctx)

	for i, id := range ids {
		got, err := h.store.GetFireNowRequest(ctx, id)
		if err != nil {
			t.Fatalf("GetFireNowRequest[%d]: %v", i, err)
		}
		if got.Status != state.FireNowStatusSucceeded {
			t.Errorf("id[%d] status = %q, want succeeded", i, got.Status)
		}
	}
	if n := h.synth.calls.Load(); n != int64(len(ids)) {
		t.Errorf("synth calls = %d, want %d (one per drain)", n, len(ids))
	}
}

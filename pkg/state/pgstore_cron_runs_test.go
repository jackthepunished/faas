package state_test

// PgStore coverage for the issue #791 cron run-history surface.
//
// MemStore has a parallel suite (memstore_cron_runs_test.go) covering
// the same semantics; this file exercises the *SQL*: that the outcome
// column is actually written by the UPDATE statements, that the
// CASE in FailInvocation's budget branch clears vs. stamps outcome
// correctly, and that ListCronRunsForCron's cron_id predicate and
// correlated-subselect cursor behave against a real planner.
//
// Sub-tests under two top-level funcs so the package's timeout budget
// stays predictable (many top-level TestPg_ funcs each pay full
// schema setup).

import (
	"context"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/state"
)

// seedCronRunPg enqueues one cron-sourced row and claims it, leaving
// it dispatching so the caller picks the terminal transition.
func seedCronRunPg(t *testing.T, s *state.PgStore, ctx context.Context, appID, acctID, cronID string, createdAt time.Time) state.Invocation {
	t.Helper()
	id := cronID
	inv, err := s.EnqueueInvocation(ctx, state.Invocation{
		AppID:     appID,
		AccountID: acctID,
		Source:    state.InvocationCron,
		Method:    "POST",
		Path:      "/cron",
		CronID:    &id,
		DueAt:     createdAt,
		CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("EnqueueInvocation: %v", err)
	}
	if _, err := s.ClaimInvocation(ctx, inv.ID, "", 30); err != nil {
		t.Fatalf("ClaimInvocation: %v", err)
	}
	return inv
}

// mustCronPg creates an app-scoped cron row so the cron_id FK on
// invocations is satisfiable (the column references crons(id)).
func mustCronPg(t *testing.T, s *state.PgStore, ctx context.Context, appID, schedule string) string {
	t.Helper()
	c, err := s.CreateCron(ctx, appID, schedule, "/cron", true)
	if err != nil {
		t.Fatalf("CreateCron: %v", err)
	}
	return c.ID
}

// TestPg_InvocationOutcomeWritePath asserts the UPDATE statements in
// CompleteInvocation / FailInvocation actually persist outcome, and
// that the CHECK constraint from migration 00166 accepts every value
// the Go layer can produce.
func TestPg_InvocationOutcomeWritePath(t *testing.T) {
	s, ctx, appID, acctID := seedInvocationPg(t)
	cronID := mustCronPg(t, s, ctx, appID, "0 */6 * * *")

	readOutcome := func(t *testing.T, id string) *state.InvocationOutcome {
		t.Helper()
		got, err := s.InvocationByID(ctx, id)
		if err != nil {
			t.Fatalf("InvocationByID: %v", err)
		}
		return got.Outcome
	}

	t.Run("complete stamps success", func(t *testing.T) {
		inv := seedCronRunPg(t, s, ctx, appID, acctID, cronID, time.Now().UTC())
		if err := s.CompleteInvocation(ctx, inv.ID, nil); err != nil {
			t.Fatalf("CompleteInvocation: %v", err)
		}
		got := readOutcome(t, inv.ID)
		if got == nil || *got != state.OutcomeSuccess {
			t.Errorf("outcome = %v, want success", got)
		}
	})

	t.Run("permanent fail defaults to failed", func(t *testing.T) {
		inv := seedCronRunPg(t, s, ctx, appID, acctID, cronID, time.Now().UTC())
		if err := s.FailInvocation(ctx, inv.ID, "boom", 0, 0); err != nil {
			t.Fatalf("FailInvocation: %v", err)
		}
		got := readOutcome(t, inv.ID)
		if got == nil || *got != state.OutcomeFailed {
			t.Errorf("outcome = %v, want failed", got)
		}
	})

	t.Run("WithOutcome persists timeout", func(t *testing.T) {
		inv := seedCronRunPg(t, s, ctx, appID, acctID, cronID, time.Now().UTC())
		if err := s.FailInvocation(ctx, inv.ID, "deadline", 0, 0,
			state.WithOutcome(state.OutcomeTimeout)); err != nil {
			t.Fatalf("FailInvocation: %v", err)
		}
		got := readOutcome(t, inv.ID)
		if got == nil || *got != state.OutcomeTimeout {
			t.Errorf("outcome = %v, want timeout", got)
		}
	})

	t.Run("transient requeue nulls outcome", func(t *testing.T) {
		inv := seedCronRunPg(t, s, ctx, appID, acctID, cronID, time.Now().UTC())
		if err := s.FailInvocation(ctx, inv.ID, "blip", time.Minute, 0,
			state.WithOutcome(state.OutcomeTimeout)); err != nil {
			t.Fatalf("FailInvocation: %v", err)
		}
		got, err := s.InvocationByID(ctx, inv.ID)
		if err != nil {
			t.Fatalf("InvocationByID: %v", err)
		}
		if got.State != state.InvocationPending {
			t.Fatalf("state = %q, want pending", got.State)
		}
		if got.Outcome != nil {
			t.Errorf("outcome = %q on a re-queued row, want NULL", *got.Outcome)
		}
	})

	t.Run("dead letter arm stamps dead_letter", func(t *testing.T) {
		inv := seedCronRunPg(t, s, ctx, appID, acctID, cronID, time.Now().UTC())
		// Claim already bumped attempts to 1; budget=1 routes the
		// transient branch's CASE to the dead-letter arm, which must
		// win over the caller's hint.
		if err := s.FailInvocation(ctx, inv.ID, "spent", time.Minute, 1,
			state.WithOutcome(state.OutcomeTimeout)); err != nil {
			t.Fatalf("FailInvocation: %v", err)
		}
		got, err := s.InvocationByID(ctx, inv.ID)
		if err != nil {
			t.Fatalf("InvocationByID: %v", err)
		}
		if got.State != state.InvocationDeadLetter {
			t.Fatalf("state = %q, want dead_letter", got.State)
		}
		if got.Outcome == nil || *got.Outcome != state.OutcomeDeadLetter {
			t.Errorf("outcome = %v, want dead_letter", got.Outcome)
		}
	})
}

// TestPg_ListCronRunsForCron pins the read SQL: the cron_id predicate,
// the newest-first ordering, and the correlated-subselect cursor.
func TestPg_ListCronRunsForCron(t *testing.T) {
	s, ctx, appID, acctID := seedInvocationPg(t)
	cronA := mustCronPg(t, s, ctx, appID, "0 */6 * * *")
	cronB := mustCronPg(t, s, ctx, appID, "0 * * * *")

	base := time.Now().UTC().Add(-6 * time.Hour)
	var aIDs []string
	for i := range 4 {
		inv := seedCronRunPg(t, s, ctx, appID, acctID, cronA, base.Add(time.Duration(i)*time.Hour))
		if err := s.CompleteInvocation(ctx, inv.ID, nil); err != nil {
			t.Fatalf("CompleteInvocation: %v", err)
		}
		aIDs = append(aIDs, inv.ID)
	}
	seedCronRunPg(t, s, ctx, appID, acctID, cronB, base.Add(30*time.Minute))

	t.Run("filters by cron id", func(t *testing.T) {
		rows, err := s.ListCronRunsForCron(ctx, cronA, 50, "")
		if err != nil {
			t.Fatalf("ListCronRunsForCron: %v", err)
		}
		if len(rows) != len(aIDs) {
			t.Fatalf("got %d rows, want %d (cron B leaked?)", len(rows), len(aIDs))
		}
		for _, r := range rows {
			if r.CronID == nil || *r.CronID != cronA {
				t.Errorf("row %s has cron_id %v, want %s", r.ID, r.CronID, cronA)
			}
		}
	})

	t.Run("orders newest first", func(t *testing.T) {
		rows, err := s.ListCronRunsForCron(ctx, cronA, 50, "")
		if err != nil {
			t.Fatalf("ListCronRunsForCron: %v", err)
		}
		for i := 1; i < len(rows); i++ {
			if rows[i-1].CreatedAt.Before(rows[i].CreatedAt) {
				t.Fatalf("row %d (%v) older than row %d (%v); want DESC",
					i-1, rows[i-1].CreatedAt, i, rows[i].CreatedAt)
			}
		}
	})

	t.Run("cursor returns strictly older rows", func(t *testing.T) {
		first, err := s.ListCronRunsForCron(ctx, cronA, 2, "")
		if err != nil {
			t.Fatalf("page 1: %v", err)
		}
		if len(first) != 2 {
			t.Fatalf("page 1 len = %d, want 2", len(first))
		}
		next, err := s.ListCronRunsForCron(ctx, cronA, 2, first[1].ID)
		if err != nil {
			t.Fatalf("page 2: %v", err)
		}
		if len(next) != 2 {
			t.Fatalf("page 2 len = %d, want 2", len(next))
		}
		for _, a := range first {
			for _, b := range next {
				if a.ID == b.ID {
					t.Fatalf("row %s appeared on both pages", a.ID)
				}
			}
		}
		if !first[1].CreatedAt.After(next[0].CreatedAt) {
			t.Errorf("cursor did not advance: tail %v, next head %v",
				first[1].CreatedAt, next[0].CreatedAt)
		}
	})

	t.Run("unknown cron id returns no rows", func(t *testing.T) {
		rows, err := s.ListCronRunsForCron(ctx, "00000000-0000-0000-0000-0000000000ff", 10, "")
		if err != nil {
			t.Fatalf("ListCronRunsForCron: %v", err)
		}
		if len(rows) != 0 {
			t.Errorf("got %d rows for an unknown cron, want 0", len(rows))
		}
	})
}

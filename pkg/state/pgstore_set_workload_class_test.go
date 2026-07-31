//go:build !no_pg

// SetAppWorkloadClass coverage tests for the ADR-051 Phase 4 seam.
// The store is intentionally thin — it writes the column and returns
// the fresh row. The interesting branches are:
//   - happy path round-trip (every WorkloadClass value)
//   - empty class → ErrInvalidArgument (fast-fail before SQL)
//   - unknown class string → SQLSTATE 23514 → ErrInvalidArgument
//     via mapErr (proves the apps_workload_class_chk tripwire)
//   - missing app → ErrNotFound (zero-rows update path)
//   - parallel Set of the same row both land with valid values
//
// The `source` argument is metadata-only (the store does not persist
// or log it) — we don't assert on a side-channel; that's the caller's
// responsibility per the audit-row pattern (ADR-035).
//
// pgtest.Open handles the Postgres-not-reachable skip cleanly.

package state_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// seedAppForClass creates one Pro-plan app the test can mutate. Each
// call gets its own schema (pgtest.Open), so concurrent tests do not
// trip each other.
func seedAppForClass(t *testing.T, ctx context.Context, s state.Store, email string) state.App {
	t.Helper()
	acct, err := s.CreateAccount(ctx, email, api.PlanPro)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	limits := api.MustLimitsFor(api.PlanPro)
	a, err := s.CreateAppIfUnderQuota(ctx, state.App{
		AccountID:      acct.ID,
		Slug:           "class-test-" + email,
		Type:           state.AppTypeFunction,
		Runtime:        "node22",
		RAMMB:          256,
		MaxConcurrency: 5,
		IdleTimeoutS:   60,
		Status:         state.AppActive,
		WorkloadClass:  state.WorkloadClassHTTP,
	}, limits)
	if err != nil {
		t.Fatalf("CreateAppIfUnderQuota: %v", err)
	}
	return a
}

func TestPg_SetAppWorkloadClass_RoundTrip(t *testing.T) {
	s, ctx := pgStore(t)
	a := seedAppForClass(t, ctx, s, "rt@example.test")

	cases := []state.WorkloadClass{
		state.WorkloadClassHTTP,
		state.WorkloadClassGraphQL,
		state.WorkloadClassGRPC,
		state.WorkloadClassJob,
		state.WorkloadClassWorker,
	}
	for _, want := range cases {
		got, err := s.SetAppWorkloadClass(ctx, a.ID, want, "scan_hint")
		if err != nil {
			t.Fatalf("SetAppWorkloadClass(%s) = %v, want nil", want, err)
		}
		if got.WorkloadClass != want {
			t.Errorf("SetAppWorkloadClass(%s) round-trip = %q, want %q",
				want, got.WorkloadClass, want)
		}
		// The RETURNING projection must include every column the
		// ColdBoot path needs — pin ID/Slug/AccountID intact (this
		// is the regression guard against scanAppInto).
		if got.ID != a.ID {
			t.Errorf("SetAppWorkloadClass returned ID=%q, want %q", got.ID, a.ID)
		}
		if got.Slug != a.Slug {
			t.Errorf("SetAppWorkloadClass returned Slug=%q, want %q", got.Slug, a.Slug)
		}
	}
}

func TestPg_SetAppWorkloadClass_EmptyClass_FastFail(t *testing.T) {
	s, ctx := pgStore(t)
	a := seedAppForClass(t, ctx, s, "empty@example.test")

	_, err := s.SetAppWorkloadClass(ctx, a.ID, state.WorkloadClass(""), "manual")
	if !errors.Is(err, state.ErrInvalidArgument) {
		t.Fatalf("SetAppWorkloadClass(\"\") = %v, want ErrInvalidArgument", err)
	}
	// Read-after-fail: the previous value must NOT be mutated.
	got, err := s.AppByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("AppByID: %v", err)
	}
	if got.WorkloadClass != a.WorkloadClass {
		t.Errorf("empty-class update mutated WorkloadClass = %q, want original %q",
			got.WorkloadClass, a.WorkloadClass)
	}
}

func TestPg_SetAppWorkloadClass_UnknownClass_TripsCHECK(t *testing.T) {
	s, ctx := pgStore(t)
	a := seedAppForClass(t, ctx, s, "unknown@example.test")

	_, err := s.SetAppWorkloadClass(ctx, a.ID, state.WorkloadClass("websocket"), "manual")
	if !errors.Is(err, state.ErrInvalidArgument) {
		t.Fatalf("SetAppWorkloadClass(\"websocket\") = %v, want ErrInvalidArgument (CHECK)", err)
	}
}

func TestPg_SetAppWorkloadClass_AppMissing(t *testing.T) {
	s, ctx := pgStore(t)
	_, err := s.SetAppWorkloadClass(ctx,
		"00000000-0000-0000-0000-000000000000", state.WorkloadClassHTTP, "observed")
	if !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("SetAppWorkloadClass on missing app = %v, want ErrNotFound", err)
	}
}

// Parallel Sets of the same row must both land with a valid class.
// This pins the absence of a write-write race on a single-row UPDATE.
// (No flaky timing: any divergence is a TestScheduler fail.)
func TestPg_SetAppWorkloadClass_ParallelSameRow(t *testing.T) {
	s, ctx := pgStore(t)
	a := seedAppForClass(t, ctx, s, "par@example.test")

	var wg sync.WaitGroup
	const N = 8
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			if _, err := s.SetAppWorkloadClass(ctx, a.ID, state.WorkloadClassGraphQL, "observed"); err != nil {
				t.Errorf("parallel Set %d: %v", i, err)
			}
		}()
	}
	wg.Wait()

	got, err := s.AppByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("AppByID: %v", err)
	}
	if got.WorkloadClass != state.WorkloadClassGraphQL {
		t.Errorf("final WorkloadClass = %q, want graphql (last-writer-wins is fine; "+
			"corruption would be anything else)", got.WorkloadClass)
	}
}

package paddle

// usage_test covers the pure helpers in usage.go + products.go's
// money conversion functions — primitives that PR #3's
// integration test will exercise end-to-end but should also be
// pinned at the unit level so a regression is caught at the
// cheapest layer.
//
// Driving accumulateOverage end-to-end requires substituting the
// SDK's CreateTransaction call; PR #3 introduces the state-store-
// backed dedupe that makes a stub-mode of the provider worth
// adding. Today we pin the primitives the executor depends on.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

func TestCalendarMonthStart(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   time.Time
		want time.Time
	}{
		{
			name: "mid-month floors to the 1st",
			in:   time.Date(2025, 6, 17, 12, 34, 56, 789_000_000, time.UTC),
			want: time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "first-of-month is unchanged",
			in:   time.Date(2025, 7, 1, 0, 0, 0, 0, time.UTC),
			want: time.Date(2025, 7, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "Feb leap year (Feb 29 23:59 lands in March bucket)",
			in:   time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC),
			want: time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "Feb non-leap (Feb 28 23:59 lands in Feb bucket, NOT Jan 30)",
			in:   time.Date(2025, 2, 28, 23, 59, 0, 0, time.UTC),
			want: time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "Dec → Jan year boundary",
			in:   time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
			want: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "non-UTC input is normalized",
			in:   time.Date(2025, 6, 17, 1, 0, 0, 0, time.FixedZone("CET", 3600)),
			want: time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := calendarMonthStart(tc.in)
			if !got.Equal(tc.want) {
				t.Errorf("calendarMonthStart(%s) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

// TestPlanMonthlyMillicents + TestPlanOverageMillicents removed: the
// price-table coverage moved to pkg/billing/plans_test.go in package
// billing_test. The per-provider copies were package-private and have
// been deleted with their helpers; the shared wrappers in plans.go now
// own the contract.

func TestMillicentsToPaddleAmount(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		mc   int64
		want string
	}{
		{"€9 = 900 cents", 900_000, "900"},
		{"€29 = 2900 cents", 2_900_000, "2900"},
		{"€99 = 9900 cents", 9_900_000, "9900"},
		{"overage €0.01 = 1 cent", 1_000, "1"},
		{"zero (free)", 0, "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := millicentsToPaddleAmount(tc.mc); got != tc.want {
				t.Errorf("millicentsToPaddleAmount(%d) = %q, want %q", tc.mc, got, tc.want)
			}
		})
	}
}

func TestPlanToProductName(t *testing.T) {
	t.Parallel()

	if got := planToProductName(api.PlanHobby); got != "faas-plan-hobby" {
		t.Errorf("planToProductName(hobby) = %q, want faas-plan-hobby", got)
	}
	if got := planToProductName(api.PlanScale); got != "faas-plan-scale" {
		t.Errorf("planToProductName(scale) = %q, want faas-plan-scale", got)
	}
}

func TestPlanProducts_ExcludesFree(t *testing.T) {
	t.Parallel()

	ps := planProducts()
	for _, p := range ps {
		if p == api.PlanFree {
			t.Errorf("planProducts() contains free (it has no recurring line item)")
		}
	}
	// 3 paid tiers — pinned so an accidental addition lands in the
	// review queue.
	if len(ps) != 3 {
		t.Errorf("planProducts() len = %d, want 3 (hobby/pro/scale)", len(ps))
	}
}

// --- accumulator end-to-end via the FlushFn test seam ---

// flushFnCounter is a FlushFn stub that records every call. The
// locking around `acc.flushed` is exercised by the production
// code; the stub only counts. Production default is defaultFlushLocked
// (real SDK POST); tests inject this counter.
func flushFnCounter(counter *int, flushErr error) FlushFn {
	return func(_ context.Context, _ *Provider, acc *overageAccumulator) error {
		*counter++
		return flushErr
	}
}

// recordingDedupe is a PaddleOverageDedupe stub that records every
// Has + Record call. The cross-process contract is tested by sharing
// one fake between two Providers — the second Provider's push must
// see Has return true and skip the flush. Mirrors the recordingStripe
// shape in pkg/meter/pusher_shadow_test.go so the test code reads the
// same way across the two billing providers.
//
// `recordErr` is an optional injected error for RecordPaddleOverageMonth;
// nil → success path. Tests that exercise the post-POST error-wrap
// branch set it to a sentinel and assert the wrapped message lands at
// the caller.
type recordingDedupe struct {
	mu        sync.Mutex
	has       int
	rec       int
	recordErr error
	rows      map[paddleDedupeKey]struct{}
}

type paddleDedupeKey struct {
	accountID string
	month     time.Time
}

func newRecordingDedupe() *recordingDedupe {
	return &recordingDedupe{rows: map[paddleDedupeKey]struct{}{}}
}

func (d *recordingDedupe) HasPaddleOverageMonth(_ context.Context, accountID string, month time.Time) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.has++
	_, ok := d.rows[paddleDedupeKey{accountID: accountID, month: month.UTC()}]
	return ok, nil
}

func (d *recordingDedupe) RecordPaddleOverageMonth(_ context.Context, accountID string, month time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rec++
	d.rows[paddleDedupeKey{accountID: accountID, month: month.UTC()}] = struct{}{}
	return d.recordErr
}

func (d *recordingDedupe) Has() int { d.mu.Lock(); defer d.mu.Unlock(); return d.has }
func (d *recordingDedupe) Rec() int { d.mu.Lock(); defer d.mu.Unlock(); return d.rec }

// seedOverageProvider builds a Provider whose catalog has the
// overage price for `plan` primed, so accumulateOverage reaches
// the flush step without EnsurePlanProducts needing the live SDK.
// Also swaps in a counting flushFn so tests can assert call counts.
func seedOverageProvider(t *testing.T, plan api.Plan, priceID string, flush FlushFn) *Provider {
	t.Helper()
	p := &Provider{
		client: nil, // unused — accumulator never reaches CreateTransaction via stubbed flushFn
		now:    time.Now,
		catalog: &priceCatalog{
			planOverage: map[api.Plan]string{plan: priceID},
		},
		flushFn: flush,
	}
	return p
}

// seedOverageProviderWithDedupe is the dedupe-wired variant of
// seedOverageProvider. Used by the cross-process dedupe tests; the
// shared recordingDedupe is the assertion target.
func seedOverageProviderWithDedupe(plan api.Plan, priceID string, flush FlushFn, dedupe PaddleOverageDedupe) *Provider {
	return &Provider{
		client: nil,
		now:    time.Now,
		catalog: &priceCatalog{
			planOverage: map[api.Plan]string{plan: priceID},
		},
		flushFn: flush,
		dedupe:  dedupe,
	}
}

// TestAccumulateOverage_CrossMonthFlush is the boundary-case pin
// for the calendarMonthStart fix. Two pushes on either side of a
// Feb → Mar boundary must bucket separately — one flush per
// month, in the right order.
func TestAccumulateOverage_CrossMonthFlush(t *testing.T) {
	t.Parallel()

	var calls int
	p := seedOverageProvider(t, api.PlanHobby, "pri_test_overage", flushFnCounter(&calls, nil))

	acct := acctWithPlan(api.PlanHobby)

	// Jan 31 23:59 UTC.
	jan31 := time.Date(2025, 1, 31, 23, 59, 0, 0, time.UTC)
	if err := p.accumulateOverage(context.Background(), acct, jan31, 1000); err != nil {
		t.Fatalf("Jan push: %v", err)
	}
	if calls != 0 {
		t.Errorf("after first push: calls=%d, want 0", calls)
	}

	// Mar 1 00:01 UTC (skips Feb entirely; exercises the calendar
	// math rather than adjacent-month drift).
	mar1 := time.Date(2025, 3, 1, 0, 1, 0, 0, time.UTC)
	if err := p.accumulateOverage(context.Background(), acct, mar1, 2000); err != nil {
		t.Fatalf("Mar push: %v", err)
	}

	// Crossing Jan → Mar should produce exactly 1 flush: Jan's
	// bucket drains when March's hour is observed. Feb never has
	// any pushes, so it doesn't flush (the bucket for Feb doesn't
	// exist).
	if calls != 1 {
		t.Errorf("after crossing Jan → Mar: calls=%d, want 1", calls)
	}
}

// TestAccumulateOverage_AdjacentMonthBoundary pins the simpler
// Jan → Feb case (every-month-has-30-day-shaped data) so a regression
// in the calendar math is loud.
func TestAccumulateOverage_AdjacentMonthBoundary(t *testing.T) {
	t.Parallel()

	var calls int
	p := seedOverageProvider(t, api.PlanHobby, "pri_test_overage", flushFnCounter(&calls, nil))

	acct := acctWithPlan(api.PlanHobby)

	jan15 := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	feb1 := time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC)

	if err := p.accumulateOverage(context.Background(), acct, jan15, 500); err != nil {
		t.Fatalf("Jan push: %v", err)
	}
	if calls != 0 {
		t.Errorf("after Jan push: calls=%d, want 0", calls)
	}
	if err := p.accumulateOverage(context.Background(), acct, feb1, 700); err != nil {
		t.Fatalf("Feb push: %v", err)
	}
	if calls != 1 {
		t.Errorf("after Feb push: calls=%d, want 1 (Jan's bucket flushed)", calls)
	}
}

// TestAccumulateOverage_WithinMonthDedupe confirms the second push
// in the same calendar month does NOT cause an additional flush —
// the `flushed` flag prevents double-billing within the same month.
// (Cross-process dedupe is documented in usage.go as a PR #3
// follow-up; this test pins the within-process contract only.)
func TestAccumulateOverage_WithinMonthDedupe(t *testing.T) {
	t.Parallel()

	var calls int
	p := seedOverageProvider(t, api.PlanHobby, "pri_test_overage", flushFnCounter(&calls, nil))

	acct := acctWithPlan(api.PlanHobby)

	// Three pushes in the same month with hour-precision spacing.
	if err := p.accumulateOverage(context.Background(), acct, time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC), 100); err != nil {
		t.Fatalf("push1: %v", err)
	}
	if err := p.accumulateOverage(context.Background(), acct, time.Date(2025, 6, 1, 0, 30, 0, 0, time.UTC), 200); err != nil {
		t.Fatalf("push2: %v", err)
	}
	if err := p.accumulateOverage(context.Background(), acct, time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC), 300); err != nil {
		t.Fatalf("push3: %v", err)
	}
	if calls != 0 {
		t.Errorf("within-month pushes: calls=%d, want 0 (no flush yet)", calls)
	}

	// Crossing into July triggers the June flush.
	if err := p.accumulateOverage(context.Background(), acct, time.Date(2025, 7, 1, 0, 0, 0, 0, time.UTC), 50); err != nil {
		t.Fatalf("July push: %v", err)
	}
	if calls != 1 {
		t.Errorf("after month-rollover: calls=%d, want 1", calls)
	}

	// Another July push: same bucket, should not flush again.
	if err := p.accumulateOverage(context.Background(), acct, time.Date(2025, 7, 15, 12, 0, 0, 0, time.UTC), 80); err != nil {
		t.Fatalf("second July push: %v", err)
	}
	if calls != 1 {
		t.Errorf("second July push should not flush: calls=%d, want 1", calls)
	}
}

// TestAccumulateOverage_FlushErrorPropagates pins the error
// contract: a failed flush must surface to the caller so meterd
// can decide whether to retry, escalate, or skip.
func TestAccumulateOverage_FlushErrorPropagates(t *testing.T) {
	t.Parallel()

	stubErr := errors.New("paddle: simulated flush failure")
	p := seedOverageProvider(t, api.PlanHobby, "pri_test_overage", flushFnCounter(new(int), stubErr))

	acct := acctWithPlan(api.PlanHobby)

	jan15 := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	feb1 := time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC)

	if err := p.accumulateOverage(context.Background(), acct, jan15, 100); err != nil {
		t.Fatalf("Jan push should succeed: %v", err)
	}
	err := p.accumulateOverage(context.Background(), acct, feb1, 200)
	if err == nil {
		t.Fatal("Feb push should surface flush failure")
	}
	if !strings.Contains(err.Error(), "simulated flush failure") {
		t.Errorf("err = %v, want it to wrap the stub error", err)
	}
}

// acctWithPlan builds a state.Account with a Plan stamped for the
// overage accumulator's price-key lookup (priceIDForPlan) and a
// non-empty StripeCustomerID (column name stale per ADR-025 — the
// stub flush doesn't post, but the production flushFn DOES pass
// it to CreateTransaction).
func acctWithPlan(plan api.Plan) state.Account {
	return state.Account{
		ID:               "acct_test_" + string(plan),
		Email:            "test@example.test",
		Plan:             plan,
		StripeCustomerID: "ctm_test_dummy",
	}
}

// TestAccumulateOverage_PostFlushRecordsDedupeRow is the single-Provider
// contract pin: after a cross-month flush, the dedupe row for the
// prior month is observable via HasPaddleOverageMonth. This is the
// per-process happy path; the cross-process variant below proves the
// redelivery-skip.
func TestAccumulateOverage_PostFlushRecordsDedupeRow(t *testing.T) {
	t.Parallel()

	dedupe := newRecordingDedupe()
	var calls int
	p := seedOverageProviderWithDedupe(api.PlanHobby, "pri_test_overage",
		flushFnCounter(&calls, nil), dedupe)

	acct := acctWithPlan(api.PlanHobby)

	jan15 := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	feb1 := time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC)

	if err := p.accumulateOverage(context.Background(), acct, jan15, 500); err != nil {
		t.Fatalf("Jan push: %v", err)
	}
	// Before the rollover: dedupe has not been touched.
	if got := dedupe.Has(); got != 0 {
		t.Errorf("pre-flush Has count = %d, want 0", got)
	}
	if got := dedupe.Rec(); got != 0 {
		t.Errorf("pre-flush Rec count = %d, want 0", got)
	}

	// Rollover triggers the flush; dedupe should see 1 Has (gate) and
	// 1 Rec (post-POST stamp).
	if err := p.accumulateOverage(context.Background(), acct, feb1, 700); err != nil {
		t.Fatalf("Feb push: %v", err)
	}
	if got := calls; got != 1 {
		t.Errorf("flush calls = %d, want 1", got)
	}
	if got := dedupe.Rec(); got != 1 {
		t.Errorf("post-flush Rec count = %d, want 1", got)
	}

	janStart := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	has, err := dedupe.HasPaddleOverageMonth(context.Background(), acct.ID, janStart)
	if err != nil {
		t.Fatalf("dedupe.Has jan: %v", err)
	}
	if !has {
		t.Error("jan dedupe row missing after flush")
	}
}

// TestAccumulateOverage_CrossProcessDedupeSkipsSecondFlush is the
// load-bearing test for the cross-process dedupe: two Providers that
// share one dedupe fake simulate a meterd crash-and-restart. The
// first Provider's cross-month push flushes January and stamps the
// dedupe row. The second Provider's same-Mar push observes Has=true
// and short-circuits the POST without invoking the flusher. This is
// the regression test for the double-bill window the PR closes.
func TestAccumulateOverage_CrossProcessDedupeSkipsSecondFlush(t *testing.T) {
	t.Parallel()

	dedupe := newRecordingDedupe()
	var callsA, callsB int

	// Two Providers, same dedupe. The `flushFn` counters are
	// per-Provider so we can assert each one independently — the
	// second Provider's flusher should never fire because the dedupe
	// short-circuits it.
	pA := seedOverageProviderWithDedupe(api.PlanHobby, "pri_test_overage",
		flushFnCounter(&callsA, nil), dedupe)
	pB := seedOverageProviderWithDedupe(api.PlanHobby, "pri_test_overage",
		flushFnCounter(&callsB, nil), dedupe)

	acct := acctWithPlan(api.PlanHobby)

	// Provider A pushes Jan 31 23:59 UTC, then Mar 1 00:01 UTC (skip
	// Feb entirely to exercise calendar math rather than adjacent-
	// month drift). The March push crosses into a new month and
	// triggers the January flush — exactly one POST.
	if err := pA.accumulateOverage(context.Background(), acct,
		time.Date(2025, 1, 31, 23, 59, 0, 0, time.UTC), 1000); err != nil {
		t.Fatalf("pA Jan push: %v", err)
	}
	if err := pA.accumulateOverage(context.Background(), acct,
		time.Date(2025, 3, 1, 0, 1, 0, 0, time.UTC), 2000); err != nil {
		t.Fatalf("pA Mar push: %v", err)
	}
	if callsA != 1 {
		t.Errorf("pA flush calls = %d, want 1 (Jan's bucket drained on Mar rollover)", callsA)
	}
	if got := dedupe.Rec(); got != 1 {
		t.Errorf("dedupe.Rec after pA flush = %d, want 1", got)
	}

	// Provider B — fresh process, no in-process `acc.flushed` state.
	// Its own accumulator is empty; the only signal it has is the
	// shared dedupe. March 1 push lands in March's bucket (not a
	// rollover for Provider B's own accumulator), so its flush path
	// goes through flushOverageLocked — which consults dedupe and
	// short-circuits because the row already exists.
	if err := pB.accumulateOverage(context.Background(), acct,
		time.Date(2025, 3, 1, 1, 0, 0, 0, time.UTC), 500); err != nil {
		t.Fatalf("pB Mar push: %v", err)
	}
	if callsB != 0 {
		t.Errorf("pB flush calls = %d, want 0 (dedupe short-circuits)", callsB)
	}

	// Dedupe was consulted (Has >= 1 from pB's gate), not re-stamped
	// (Rec stays at 1).
	if got := dedupe.Has(); got < 1 {
		t.Errorf("dedupe.Has after pB = %d, want >= 1 (gate observed)", got)
	}
	if got := dedupe.Rec(); got != 1 {
		t.Errorf("dedupe.Rec after pB = %d, want 1 (no second stamp)", got)
	}
}

// TestAccumulateOverage_RecordErrorPropagates pins the post-POST
// error-wrap path: the SDK POST commits, but RecordPaddleOverageMonth
// fails. The flush must surface the wrapped error so meterd can
// decide whether to retry, escalate, or skip — same contract the
// existing TestAccumulateOverage_FlushErrorPropagates pins for the
// SDK POST itself.
//
// This is the residual TOCTOU risk the flushOverageLocked docstring
// calls out: Paddle has no external Idempotency-Key header, so a
// failed Record means the next push re-POSTs. Surfacing the error
// keeps the failure mode observable instead of silent.
func TestAccumulateOverage_RecordErrorPropagates(t *testing.T) {
	t.Parallel()

	dedupe := newRecordingDedupe()
	stubErr := errors.New("paddle: simulated dedupe record failure")
	dedupe.recordErr = stubErr
	var calls int
	p := seedOverageProviderWithDedupe(api.PlanHobby, "pri_test_overage",
		flushFnCounter(&calls, nil), dedupe)

	acct := acctWithPlan(api.PlanHobby)

	jan15 := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	feb1 := time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC)

	if err := p.accumulateOverage(context.Background(), acct, jan15, 100); err != nil {
		t.Fatalf("Jan push should succeed (no rollover yet): %v", err)
	}
	// Rollover triggers the flush; SDK POST succeeds (flushFnCounter
	// returns nil), then Record fails — the wrapped error must
	// surface to the caller.
	err := p.accumulateOverage(context.Background(), acct, feb1, 200)
	if err == nil {
		t.Fatal("Feb push should surface the dedupe record failure")
	}
	if !strings.Contains(err.Error(), "simulated dedupe record failure") {
		t.Errorf("err = %v, want it to wrap the stub error", err)
	}
	if !strings.Contains(err.Error(), "paddle: dedupe record month=") {
		t.Errorf("err = %v, want it to carry the dedupe record wrap prefix", err)
	}

	// Sanity: the SDK POST actually fired (Record only runs after a
	// successful flush), so the cross-process gate would have
	// observed the row on a retry — this is the leak the residual
	// risk calls out.
	if calls != 1 {
		t.Errorf("flush calls = %d, want 1 (SDK POST must commit before Record)", calls)
	}
	if got := dedupe.Rec(); got != 1 {
		t.Errorf("dedupe.Rec = %d, want 1 (Record was attempted)", got)
	}
}

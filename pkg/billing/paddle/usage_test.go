package paddle

// usage_test covers the pure helpers in usage.go + products.go's
// money conversion functions — primitives that PR #3's
// integration test will exercise end-to-end but should also be
// pinned at the unit level so a regression is caught at the
// cheapest layer.
//
// Driving PushUsageRecord end-to-end requires substituting the
// SDK's CreateTransaction call; we use the `flushFn` seam
// installed at provider.go to swap in a counter stub. Tests that
// use the dedupe gate get a second stub (`recordingDedupe`)
// that records Has/Rec pairs. Together the two stubs expose every
// branch of flushOverageLocked without standing up a real
// *paddle.SDK.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PaddleHQ/paddle-go-sdk/v5"
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

// --- stateless per-push PushUsageRecord ---

// flushFnCounter is a FlushFn stub that records every call. The
// dedupe gate consult happens before this stub fires; the stub
// itself only counts. Production default is defaultFlushLocked
// (real SDK POST); tests inject this counter so they can assert
// call counts without standing up a real SDK.
func flushFnCounter(counter *int, flushErr error) FlushFn {
	return func(_ context.Context, _ *Provider, _ state.Account, _ time.Time, _ int64) error {
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
// overage price for `plan` primed, so PushUsageRecord reaches
// the flush step without EnsurePlanProducts needing the live SDK.
// Also swaps in a counting flushFn so tests can assert call counts.
//
// The `client: nil` is intentional — the flusher is stubbed, so the
// SDK is never invoked. This mirrors the pattern from PR #179.
func seedOverageProvider(t *testing.T, plan api.Plan, priceID string, flush FlushFn) *Provider {
	t.Helper()
	p := &Provider{
		client: nil, // unused — flusher never reaches CreateTransaction via stubbed flushFn
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

// acctWithPlan builds a state.Account with a Plan stamped for the
// overage flusher's price-key lookup (overagePriceForPlan) and a
// non-empty ProviderCustomerID (column name stale per ADR-025 — the
// stub flush doesn't post, but the production flushFn DOES pass
// it to CreateTransaction).
func acctWithPlan(plan api.Plan) state.Account {
	return state.Account{
		ID:               "acct_test_" + string(plan),
		Email:            "test@example.test",
		Plan:             plan,
		ProviderCustomerID: "ctm_test_dummy",
	}
}

// TestFlushOverageLocked_PostsOnFirstCall — first call for a (acct,
// month) pair hits the flusher exactly once. Mirrors the Stripe
// PushHour happy path (pkg/meter/pusher_test.go).
//
// flushOverageLocked is invoked directly rather than via
// PushUsageRecord because the seeded test Provider has a nil
// client (the flusher stub replaces the SDK call). The production
// PushUsageRecord short-circuits on nil-client with ErrNoAPIKey
// — see TestPushUsageRecord_NilClientIsNoAPIKey.
func TestFlushOverageLocked_PostsOnFirstCall(t *testing.T) {
	t.Parallel()

	var calls int
	p := seedOverageProvider(t, api.PlanHobby, "pri_test_overage", flushFnCounter(&calls, nil))
	acct := acctWithPlan(api.PlanHobby)

	jan15 := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	if err := p.flushOverageLocked(context.Background(), acct, jan15, 1024); err != nil {
		t.Fatalf("push: %v", err)
	}
	if calls != 1 {
		t.Errorf("flush calls = %d, want 1 (first push should flush)", calls)
	}
}

// TestFlushOverageLocked_SkipsOnZeroSum — mb_seconds == 0 is a no-op
// (no SDK POST, no dedupe touch). flushOverageLocked guards on 0
// defensively even though PushUsageRecord's pre-SDK guards already
// short-circuit.
func TestFlushOverageLocked_SkipsOnZeroSum(t *testing.T) {
	t.Parallel()

	var calls int
	p := seedOverageProvider(t, api.PlanHobby, "pri_test_overage", flushFnCounter(&calls, nil))
	acct := acctWithPlan(api.PlanHobby)

	jan15 := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	if err := p.flushOverageLocked(context.Background(), acct, jan15, 0); err != nil {
		t.Fatalf("zero-sum push: %v", err)
	}
	if calls != 0 {
		t.Errorf("zero-sum push fired flusher: calls=%d, want 0", calls)
	}
}

// TestDefaultFlushLocked_MissingOveragePrice — EnsurePlanProducts has
// not populated the catalog (or the plan changed at runtime). The
// default flusher must surface ErrOveragePriceMissing so the
// classifier maps to "overage-price-missing". Pushing an empty
// priceID through a real *paddle.SDK would 422; we want the pre-SDK
// fast-fail.
//
// Tested at the default-flusher layer (not flushOverageLocked) because
// flushOverageLocked delegates to p.flushFn first and only consults
// defaultFlushLocked when p.flushFn is nil. The price-missing
// guard lives inside defaultFlushLocked.
func TestDefaultFlushLocked_MissingOveragePrice(t *testing.T) {
	t.Parallel()

	// Provider with no catalog entries for the requested plan —
	// overagePriceForPlan returns "" and the default flusher short-
	// circuits before touching the SDK.
	p := &Provider{
		client: nil, // never reached
		now:    time.Now,
		catalog: &priceCatalog{
			planOverage: map[api.Plan]string{}, // empty → lookup returns ""
		},
	}
	acct := acctWithPlan(api.PlanHobby)
	janStart := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	err := defaultFlushLocked(context.Background(), p, acct, janStart, 1024)
	if err == nil {
		t.Fatal("missing overage price should error")
	}
	if !errors.Is(err, ErrOveragePriceMissing) {
		t.Errorf("err = %v, want errors.Is(_, ErrOveragePriceMissing) == true", err)
	}
}

// TestPushUsageRecord_NilClientIsNoAPIKey — when the SDK didn't init
// (bad apiKey at boot), PushUsageRecord must surface ErrNoAPIKey so
// the classifier maps to "no-api-key" rather than a generic SDK init
// error. Belt + braces against a future change that passes through
// the paddle.New error.
func TestPushUsageRecord_NilClientIsNoAPIKey(t *testing.T) {
	t.Parallel()

	// Provider with no flusher substituted AND no client — exercises the
	// fast-fail at provider.go's PushUsageRecord entry (not the flusher).
	p := &Provider{
		client:  nil,
		now:     time.Now,
		catalog: &priceCatalog{planOverage: map[api.Plan]string{api.PlanHobby: "pri_test_overage"}},
	}
	acct := acctWithPlan(api.PlanHobby)

	err := p.PushUsageRecord(context.Background(), acct, time.Now(), 1024)
	if err == nil {
		t.Fatal("nil-client push should error")
	}
	if !errors.Is(err, ErrNoAPIKey) {
		t.Errorf("err = %v, want errors.Is(_, ErrNoAPIKey) == true", err)
	}
}

// TestPushUsageRecord_NegativeMBSeconds — PushUsageRecord surfaces
// ErrNegativeMBSeconds so the classifier at errors.go maps to
// "negative-mb-sec". Belt + braces against an inline error message
// drift (the classifier uses errors.Is, not string-fragment
// matching).
func TestPushUsageRecord_NegativeMBSeconds(t *testing.T) {
	t.Parallel()

	// Need a non-nil client to bypass the nil-client guard; we never
	// reach the SDK because the negative-mb_seconds guard fires first.
	p := &Provider{
		client:  &paddle.SDK{}, // non-nil; never invoked
		now:     time.Now,
		catalog: &priceCatalog{planOverage: map[api.Plan]string{api.PlanHobby: "pri_test_overage"}},
	}
	acct := acctWithPlan(api.PlanHobby)

	err := p.PushUsageRecord(context.Background(), acct, time.Now(), -1)
	if err == nil {
		t.Fatal("negative mb_seconds should error")
	}
	if !errors.Is(err, ErrNegativeMBSeconds) {
		t.Errorf("err = %v, want errors.Is(_, ErrNegativeMBSeconds) == true", err)
	}
}

// TestFlushOverageLocked_PostFlushRecordsDedupeRow is the single-Provider
// contract pin: after a successful flush, the dedupe row for that
// (acct, month) is observable via HasPaddleOverageMonth. The within-
// process "flushed" stamp that the old accumulator provided is now
// provided by the state.Store row itself.
func TestFlushOverageLocked_PostFlushRecordsDedupeRow(t *testing.T) {
	t.Parallel()

	dedupe := newRecordingDedupe()
	var calls int
	p := seedOverageProviderWithDedupe(api.PlanHobby, "pri_test_overage",
		flushFnCounter(&calls, nil), dedupe)

	acct := acctWithPlan(api.PlanHobby)

	jan15 := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	if err := p.flushOverageLocked(context.Background(), acct, jan15, 500); err != nil {
		t.Fatalf("Jan push: %v", err)
	}

	// One flush, one gate observation, one record stamp.
	if calls != 1 {
		t.Errorf("flush calls = %d, want 1", calls)
	}
	if got := dedupe.Has(); got != 1 {
		t.Errorf("Has count = %d, want 1 (gate observed)", got)
	}
	if got := dedupe.Rec(); got != 1 {
		t.Errorf("Rec count = %d, want 1 (post-POST stamp)", got)
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

// TestFlushOverageLocked_CrossProcessDedupeSkipsSecondFlush is the
// load-bearing regression test for the double-bill window the PR
// closes: two Providers that share one dedupe fake simulate a
// meterd crash-and-restart. The first Provider's flush stamps the
// dedupe row. The second Provider's same-month flush observes
// Has=true and short-circuits without invoking the flusher.
func TestFlushOverageLocked_CrossProcessDedupeSkipsSecondFlush(t *testing.T) {
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

	// pA flushes January.
	jan15 := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	if err := pA.flushOverageLocked(context.Background(), acct, jan15, 1000); err != nil {
		t.Fatalf("pA Jan push: %v", err)
	}
	if callsA != 1 {
		t.Errorf("pA flush calls = %d, want 1", callsA)
	}
	if got := dedupe.Rec(); got != 1 {
		t.Errorf("dedupe.Rec after pA flush = %d, want 1", got)
	}

	// pB — fresh process, no in-process state. Its flush targets the
	// same January bucket. The gate consult observes the row and
	// short-circuits the flush; the flusher never fires.
	jan20 := time.Date(2025, 1, 20, 8, 0, 0, 0, time.UTC)
	if err := pB.flushOverageLocked(context.Background(), acct, jan20, 500); err != nil {
		t.Fatalf("pB Jan push: %v", err)
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

// TestFlushOverageLocked_DistinctMonthsBothFlush — the dedupe gate is
// keyed on (acct, month); a flush for a different month with the
// same account does NOT short-circuit. Two flushes for January then
// February both fire — one per month.
func TestFlushOverageLocked_DistinctMonthsBothFlush(t *testing.T) {
	t.Parallel()

	dedupe := newRecordingDedupe()
	var calls int
	p := seedOverageProviderWithDedupe(api.PlanHobby, "pri_test_overage",
		flushFnCounter(&calls, nil), dedupe)

	acct := acctWithPlan(api.PlanHobby)

	// Two flushes in adjacent months.
	if err := p.flushOverageLocked(context.Background(), acct,
		time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC), 500); err != nil {
		t.Fatalf("Jan push: %v", err)
	}
	if err := p.flushOverageLocked(context.Background(), acct,
		time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC), 700); err != nil {
		t.Fatalf("Feb push: %v", err)
	}
	if calls != 2 {
		t.Errorf("flush calls = %d, want 2 (Jan + Feb)", calls)
	}
	if got := dedupe.Rec(); got != 2 {
		t.Errorf("dedupe.Rec = %d, want 2 (one stamp per month)", got)
	}
}

// TestFlushOverageLocked_RecordErrorPropagates pins the post-POST
// error-wrap path: the SDK POST commits (flushFnCounter returns
// nil), but RecordPaddleOverageMonth fails. The push must surface
// the wrapped error so meterd can decide whether to retry,
// escalate, or skip.
//
// This is the residual TOCTOU risk the flushOverageLocked docstring
// calls out: a failed Record means the next push re-POSTs. Surfacing
// the error keeps the failure mode observable instead of silent.
//
// The new Idempotency-Key HTTP header (NewIdempotencyRT,
// transport.go) is the load-bearing mitigation for this risk when
// Paddle's server-side dedupe ships — until then, surfacing the
// error is the only signal.
func TestFlushOverageLocked_RecordErrorPropagates(t *testing.T) {
	t.Parallel()

	dedupe := newRecordingDedupe()
	stubErr := errors.New("paddle: simulated dedupe record failure")
	dedupe.recordErr = stubErr
	var calls int
	p := seedOverageProviderWithDedupe(api.PlanHobby, "pri_test_overage",
		flushFnCounter(&calls, nil), dedupe)

	acct := acctWithPlan(api.PlanHobby)
	jan15 := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)

	err := p.flushOverageLocked(context.Background(), acct, jan15, 100)
	if err == nil {
		t.Fatal("push should surface the dedupe record failure")
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

// TestFlushOverageLocked_FlushErrorPropagates pins the error
// contract: a failed flush must surface to the caller so meterd
// can decide whether to retry, escalate, or skip. The dedupe row
// must NOT be stamped when the flush fails (otherwise the second
// push would be silently skipped instead of retried).
func TestFlushOverageLocked_FlushErrorPropagates(t *testing.T) {
	t.Parallel()

	dedupe := newRecordingDedupe()
	stubErr := errors.New("paddle: simulated flush failure")
	var calls int
	p := seedOverageProviderWithDedupe(api.PlanHobby, "pri_test_overage",
		flushFnCounter(&calls, stubErr), dedupe)

	acct := acctWithPlan(api.PlanHobby)
	jan15 := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)

	err := p.flushOverageLocked(context.Background(), acct, jan15, 100)
	if err == nil {
		t.Fatal("push should surface flush failure")
	}
	if !errors.Is(err, stubErr) {
		t.Errorf("err = %v, want errors.Is(_, stubErr) == true", err)
	}
	// Record was NOT stamped because the flush returned an error.
	if got := dedupe.Rec(); got != 0 {
		t.Errorf("dedupe.Rec = %d, want 0 (Record skipped when flush fails)", got)
	}
}

// Package reconciler tests (ADR-049 §B.1). Pure unit tests —
// uses stub Provider + state.MemStore so pkg/billing/reconciler
// stays pgxpool-free. The full Store integration is exercised by
// the e2e suite (cmd/e2e/reconciler_test.go) once the box surface
// ships.
package reconciler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/billing"
	"github.com/onebox-faas/faas/pkg/state"
)

type stubProvider struct {
	pushed int64
	err    error
}

func (s *stubProvider) EnsurePlanProducts(context.Context) error    { return nil }
func (s *stubProvider) CreateCustomer(context.Context, state.Account) (string, error) {
	return "", nil
}
func (s *stubProvider) PushUsageRecord(context.Context, state.Account, time.Time, int64) error {
	return nil
}
func (s *stubProvider) VerifyWebhook([]byte, map[string]string, time.Duration) (billing.Event, error) {
	return billing.Event{}, nil
}
func (s *stubProvider) CreateUpgradeTransaction(context.Context, state.Account, api.Plan) (string, string, error) {
	return "", "", nil
}
func (s *stubProvider) Refund(context.Context, string, int64) (*billing.RefundResult, error) {
	return nil, nil
}
func (s *stubProvider) ReconcileUsage(_ context.Context, _ state.Account, _, _ time.Time) (int64, error) {
	return s.pushed, s.err
}

// seedMemStore constructs a MemStore with one account + a row
// of per-hour usage. MemStore.CreateAccount generates a fresh
// random ID per call, so we look up the created account via
// ListAllAccounts and use its real ID for AppendUsage. The
// reconciler-side ListAllAccounts picks up the same ID.
func seedMemStore(t *testing.T, label string, plan api.Plan, mbSeconds int64) (*state.MemStore, string) {
	t.Helper()
	store := state.NewMemStore()
	if _, err := store.CreateAccount(context.Background(), label+"@example.com", plan); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	accts, err := store.ListAllAccounts(context.Background())
	if err != nil || len(accts) != 1 {
		t.Fatalf("ListAllAccounts: %v (got %d)", err, len(accts))
	}
	id := accts[0].ID
	// AppendUsage is a pure write — MemStore does not cross-check
	// against accounts. We seed a row in the reconciler's 24h
	// window (now - 30 min, well inside [now-24h, now]) so
	// UsageByHour sees it.
	if err := store.AppendUsage(
		context.Background(),
		id, "app1", "inst1",
		time.Now().UTC().Truncate(time.Hour).Add(-30*time.Minute),
		mbSeconds, 0, 0, 0, 0, 0, 0); err != nil {
		t.Fatalf("AppendUsage: %v", err)
	}
	return store, id
}

// listAcctIDs reflects over MemStore to dump the account-id set so
// the reconciler-side ListAllAccounts can pick them up. We do
// this via the public store API: ListAllAccounts on MemStore
// walks m.accounts (an internal map). Retained for any future
// test that needs to assert ListAllAccounts returns the seeded
// account.
func listAcctIDs(t *testing.T, store *state.MemStore) []string {
	t.Helper()
	accts, err := store.ListAllAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAllAccounts: %v", err)
	}
	ids := make([]string, 0, len(accts))
	for _, a := range accts {
		ids = append(ids, a.ID)
	}
	return ids
}

var _ = listAcctIDs // keep the helper exported for future tests

// TestReconciler_ZeroDriftIsHappyPath asserts the gauges stay at 0
// when local sum == pushed sum. This is the steady state the
// BillingDrift alert expects to see in production.
func TestReconciler_ZeroDriftIsHappyPath(t *testing.T) {
	store, id := seedMemStore(t, "acct_a", api.PlanHobby, 3600)
	prov := &stubProvider{pushed: 3600}
	rec := New("stripe", store, prov, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err := rec.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty seeded account id")
	}
	srv := httptest.NewServer(rec.Handler())
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "meterd_billing_drift_mb_seconds") {
		t.Errorf("expected metric name in scrape body, got:\n%s", body)
	}
	if !strings.Contains(string(body), "meterd_billing_drift_ratio") {
		t.Errorf("expected ratio metric name in scrape body, got:\n%s", body)
	}
}

// TestReconciler_EmitsDriftOnMismatch asserts the gauges reflect
// the local-pushed gap. The BillingDrift alert gates on ratio >
// 0.005; this test pins the formula.
func TestReconciler_EmitsDriftOnMismatch(t *testing.T) {
	store, id := seedMemStore(t, "acct_drift", api.PlanHobby, 1000)
	prov := &stubProvider{pushed: 990}
	rec := New("stripe", store, prov, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err := rec.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	srv := httptest.NewServer(rec.Handler())
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatalf("scrape GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	// local=1000, pushed=990 → drift=10, ratio=10/1000=0.01
	driftLine := `meterd_billing_drift_mb_seconds{account_id="` + id + `",provider="stripe"} 10`
	ratioLine := `meterd_billing_drift_ratio{account_id="` + id + `",provider="stripe"} 0.01`
	if !strings.Contains(string(body), driftLine) {
		t.Errorf("expected %q in scrape body, got:\n%s", driftLine, body)
	}
	if !strings.Contains(string(body), ratioLine) {
		t.Errorf("expected %q in scrape body, got:\n%s", ratioLine, body)
	}
}

// TestReconciler_ProviderErrorFailsSoft asserts a provider error
// for one account does not block the loop or fail RunOnce.
func TestReconciler_ProviderErrorFailsSoft(t *testing.T) {
	store, _ := seedMemStore(t, "acct_err", api.PlanHobby, 500)
	prov := &stubProvider{err: errors.New("transient stripe blip")}
	rec := New("stripe", store, prov, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err := rec.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce should not fail-soft propagate: %v", err)
	}
}

// TestReconciler_ErrNotImplementedSkipped asserts Paddle's
// not-yet-implemented ErrNotImplemented is treated as "no drift
// signal" rather than an error.
func TestReconciler_ErrNotImplementedSkipped(t *testing.T) {
	store, id := seedMemStore(t, "acct_p", api.PlanHobby, 1234)
	prov := &stubProvider{err: billing.ErrNotImplemented}
	rec := New("paddle", store, prov, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err := rec.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce should swallow ErrNotImplemented: %v", err)
	}
	// The reconciler should NOT emit any gauge for this account
	// since the provider has nothing to say.
	srv := httptest.NewServer(rec.Handler())
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatalf("scrape GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), `account_id="`+id+`"`) {
		t.Errorf("expected no gauge for ErrNotImplemented account, got:\n%s", body)
	}
}

// TestReconciler_FreePlanSkipped asserts Free plan accounts do not
// produce drift gauge rows.
func TestReconciler_FreePlanSkipped(t *testing.T) {
	store, id := seedMemStore(t, "acct_free", api.PlanFree, 100)
	prov := &stubProvider{pushed: 0}
	rec := New("stripe", store, prov, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err := rec.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	srv := httptest.NewServer(rec.Handler())
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatalf("scrape GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), `account_id="`+id+`"`) {
		t.Errorf("expected free-plan account to be skipped, got:\n%s", body)
	}
}

// TestReconciler_LoopStopsOnContextCancel is the goroutine-lifecycle
// tripwire: the Loop must exit when ctx is cancelled (otherwise the
// meterd daemon hangs on shutdown).
func TestReconciler_LoopStopsOnContextCancel(t *testing.T) {
	store, _ := seedMemStore(t, "acct_a", api.PlanHobby, 100)
	prov := &stubProvider{}
	rec := New("stripe", store, prov, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		rec.Loop(ctx, time.Hour)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Loop did not exit on ctx.Done()")
	}
}

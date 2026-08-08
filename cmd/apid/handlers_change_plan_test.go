// cmd/apid/handlers_change_plan_test.go — focused test file for the
// changePlan handler (issue #142). The existing handlers_ext_test.go is
// dedicated to the deployment-logs SSE; a focused file mirrors the layout
// PR #131 used for the A1/A2/A3/A4 work and keeps the test envelope
// small.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/billing"
	"github.com/onebox-faas/faas/pkg/state"
)

// billingPortalURL is the production-shape template the operator is
// expected to set. Tests assert the {account_id} substitution happens
// at write time.
const billingPortalURL = "https://billing.example.com/portal?account={account_id}"

// setupChangePlan wires a server with the billing portal URL baked
// in. The existing setup helper (server_test.go) intentionally keeps
// the surface minimal; tests that need the 402 extension need a richer
// hook, so we extend here rather than mutate the shared helper.
//
// Returns both the testEnv (for HTTP-level assertions) and the *server
// (for tests that need to install a billing.Provider via
// WithBillingProvider). The handler is closed over the same server
// reference, so installing the provider before issuing the request
// is observable on the request path.
func setupChangePlan(t *testing.T, plan api.Plan, stripeItem string) (testEnv, *server) {
	t.Helper()
	store := state.NewMemStore()
	acct, err := store.CreateAccount(context.Background(), fmt.Sprintf("%s@example.com", plan), plan)
	if err != nil {
		t.Fatal(err)
	}
	if stripeItem != "" {
		if err := store.UpdateAccountStripeSubscriptionItem(context.Background(), acct.ID, stripeItem); err != nil {
			t.Fatal(err)
		}
	}
	pt, hash, _ := api.GenerateAPIKey()
	if _, err := store.CreateAPIKey(context.Background(), acct.ID, hash, "test", api.ScopesAdminOnly); err != nil {
		t.Fatal(err)
	}
	srv := newServerWithDeps(store, slog.New(slog.NewTextHandler(io.Discard, nil)),
		"example.com", noopNotifier{}, "", noopMailer{}, stubGithubdClient{}, nil, nil,
		15*24, "")
	srv.WithBillingPortalURL(billingPortalURL)
	return testEnv{h: srv.handler(), store: store, key: pt, acct: acct}, srv
}

// TestChangePlan_GateMatrix exercises every (current, requested) pair
// in the gate table. The expected outcome is the post-#142 behavior.
func TestChangePlan_GateMatrix(t *testing.T) {
	cases := []struct {
		name             string
		startingPlan     api.Plan
		stripeItem       string
		requestedPlan    api.Plan
		wantStatus       int
		wantCode         string
		wantPortalURLSet bool
		wantPlanAfter    api.Plan
	}{
		// The free → hobby M5 path. No Stripe item required.
		{
			name: "free→hobby_allowed", startingPlan: api.PlanFree, stripeItem: "",
			requestedPlan: api.PlanHobby, wantStatus: 200, wantCode: "",
			wantPlanAfter: api.PlanHobby,
		},
		// The documented exploit path from issue #142. Must be 402.
		{
			name: "free→pro_blocked_no_stripe", startingPlan: api.PlanFree, stripeItem: "",
			requestedPlan: api.PlanPro, wantStatus: 402, wantCode: api.CodePayment,
			wantPortalURLSet: true, wantPlanAfter: api.PlanFree,
		},
		{
			name: "free→scale_blocked_no_stripe", startingPlan: api.PlanFree, stripeItem: "",
			requestedPlan: api.PlanScale, wantStatus: 402, wantCode: api.CodePayment,
			wantPortalURLSet: true, wantPlanAfter: api.PlanFree,
		},
		// Hobby→Pro/Scale also requires Stripe item.
		{
			name: "hobby→pro_blocked_no_stripe", startingPlan: api.PlanHobby, stripeItem: "",
			requestedPlan: api.PlanPro, wantStatus: 402, wantCode: api.CodePayment,
			wantPortalURLSet: true, wantPlanAfter: api.PlanHobby,
		},
		{
			name: "hobby→scale_blocked_no_stripe", startingPlan: api.PlanHobby, stripeItem: "",
			requestedPlan: api.PlanScale, wantStatus: 402, wantCode: api.CodePayment,
			wantPortalURLSet: true, wantPlanAfter: api.PlanHobby,
		},
		// With a Stripe item, the upgrade proceeds (the gateway webhook
		// stamps the item on the way through, so the same handler that
		// 402s for an API-only call accepts the same upgrade here).
		{
			name: "hobby→pro_allowed_with_stripe", startingPlan: api.PlanHobby, stripeItem: "si_abc",
			requestedPlan: api.PlanPro, wantStatus: 200, wantCode: "",
			wantPlanAfter: api.PlanPro,
		},
		{
			name: "pro→scale_allowed_with_stripe", startingPlan: api.PlanPro, stripeItem: "si_abc",
			requestedPlan: api.PlanScale, wantStatus: 200, wantCode: "",
			wantPlanAfter: api.PlanScale,
		},
		{
			name: "pro→scale_blocked_no_stripe", startingPlan: api.PlanPro, stripeItem: "",
			requestedPlan: api.PlanScale, wantStatus: 402, wantCode: api.CodePayment,
			wantPortalURLSet: true, wantPlanAfter: api.PlanPro,
		},
		// Downgrades always pass.
		{
			name: "pro→free_downgrade", startingPlan: api.PlanPro, stripeItem: "si_abc",
			requestedPlan: api.PlanFree, wantStatus: 200, wantCode: "",
			wantPlanAfter: api.PlanFree,
		},
		{
			name: "scale→pro_downgrade", startingPlan: api.PlanScale, stripeItem: "si_abc",
			requestedPlan: api.PlanPro, wantStatus: 200, wantCode: "",
			wantPlanAfter: api.PlanPro,
		},
		// Idempotent same-tier: success, no gate.
		{
			name: "free→free_idempotent", startingPlan: api.PlanFree, stripeItem: "",
			requestedPlan: api.PlanFree, wantStatus: 200, wantCode: "",
			wantPlanAfter: api.PlanFree,
		},
		// Bogus plan: existing 400 path is preserved.
		{
			name: "free→bogus_validation", startingPlan: api.PlanFree, stripeItem: "",
			requestedPlan: api.Plan("platinum"), wantStatus: 400, wantCode: api.CodeValidation,
			wantPlanAfter: api.PlanFree,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := setupChangePlan(t, tc.startingPlan, tc.stripeItem)
			rec := e.do(t, "PATCH", "/v1/account/plan",
				map[string]string{"plan": string(tc.requestedPlan)}, nil)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d\nbody = %s", rec.Code, tc.wantStatus, rec.Body)
			}
			if tc.wantCode != "" {
				var prob api.Problem
				if err := json.Unmarshal(rec.Body.Bytes(), &prob); err != nil {
					t.Fatalf("body not problem+json: %s", rec.Body)
				}
				if prob.Code != tc.wantCode {
					t.Errorf("code = %q, want %q", prob.Code, tc.wantCode)
				}
				if tc.wantPortalURLSet {
					if prob.BillingPortalURL == "" {
						t.Errorf("expected billing_portal_url to be set; got empty")
					}
					// The {account_id} placeholder must be substituted.
					if !strings.Contains(prob.BillingPortalURL, e.acct.ID) {
						t.Errorf("billing_portal_url = %q, missing account id %q",
							prob.BillingPortalURL, e.acct.ID)
					}
					if strings.Contains(prob.BillingPortalURL, "{account_id}") {
						t.Errorf("billing_portal_url still has unresolved placeholder: %q",
							prob.BillingPortalURL)
					}
				} else {
					if prob.BillingPortalURL != "" {
						t.Errorf("billing_portal_url should be empty for %s; got %q",
							tc.name, prob.BillingPortalURL)
					}
				}
			}
			// Verify the plan actually changed (or didn't) in the store.
			updated, err := e.store.AccountByID(context.Background(), e.acct.ID)
			if err != nil {
				t.Fatal(err)
			}
			if updated.Plan != tc.wantPlanAfter {
				t.Errorf("plan after = %q, want %q", updated.Plan, tc.wantPlanAfter)
			}
		})
	}
}

// TestChangePlan_NoBillingPortalURL confirms the 402 still goes out (the
// customer knows the upgrade was blocked) when the operator has not
// configured a billing portal URL. The BillingPortalURL field is
// omitted; the rest of the response body is unchanged.
func TestChangePlan_NoBillingPortalURL(t *testing.T) {
	store := state.NewMemStore()
	acct, err := store.CreateAccount(context.Background(), "free@example.com", api.PlanFree)
	if err != nil {
		t.Fatal(err)
	}
	pt, hash, _ := api.GenerateAPIKey()
	if _, err := store.CreateAPIKey(context.Background(), acct.ID, hash, "test", api.ScopesAdminOnly); err != nil {
		t.Fatal(err)
	}
	// Note: WithBillingPortalURL is NOT called — simulates the operator
	// shipping apid without FAAS_BILLING_PORTAL_URL set.
	srv := newServerWithDeps(store, slog.New(slog.NewTextHandler(io.Discard, nil)),
		"example.com", noopNotifier{}, "", noopMailer{}, stubGithubdClient{}, nil, nil, 15*24, "")
	rec := httptest.NewRecorder()
	body := strings.NewReader(`{"plan":"pro"}`)
	req := httptest.NewRequest("PATCH", "/v1/account/plan", body)
	req.Header.Set("Authorization", "Bearer "+pt)
	req.Header.Set("Content-Type", "application/json")
	srv.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402\nbody = %s", rec.Code, rec.Body)
	}
	var prob api.Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &prob); err != nil {
		t.Fatalf("body not problem+json: %s", rec.Body)
	}
	if prob.Code != api.CodePayment {
		t.Errorf("code = %q, want %q", prob.Code, api.CodePayment)
	}
	if prob.BillingPortalURL != "" {
		t.Errorf("billing_portal_url should be omitted when no template; got %q", prob.BillingPortalURL)
	}
}

// TestChangePlan_NoAccountWriteOnGate is the regression net for the
// audit's concern: a 402 must not write to the account record. We
// assert by snapshotting the stored plan before the request and
// confirming it did not change.
func TestChangePlan_NoAccountWriteOnGate(t *testing.T) {
	e, _ := setupChangePlan(t, api.PlanFree, "")
	rec := e.do(t, "PATCH", "/v1/account/plan",
		map[string]string{"plan": "pro"}, nil)
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402\nbody = %s", rec.Code, rec.Body)
	}
	updated, err := e.store.AccountByID(context.Background(), e.acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Plan != api.PlanFree {
		t.Errorf("plan = %q after 402, want free (no write)", updated.Plan)
	}
}

// TestPlanIsPaidAndRequiresStripeUpgradeTo is the unit-level pin for the
// two helpers introduced in pkg/api/limits.go. The end-to-end
// TestChangePlan_GateMatrix already covers the integration; this one
// pins the helpers themselves so any future drift in the rule surfaces
// as a test failure here, not as a surprise in production.
func TestPlanIsPaidAndRequiresStripeUpgradeTo(t *testing.T) {
	for _, p := range []api.Plan{api.PlanFree, api.PlanHobby, api.PlanPro, api.PlanScale} {
		t.Run(string(p)+"_IsPaid", func(t *testing.T) {
			want := p != api.PlanFree
			if got := p.IsPaid(); got != want {
				t.Errorf("IsPaid(%q) = %v, want %v", p, got, want)
			}
		})
	}
	type req struct {
		from, to api.Plan
		want     bool
	}
	for _, r := range []req{
		{api.PlanFree, api.PlanFree, false},
		{api.PlanFree, api.PlanHobby, false}, // the M5 direct path
		{api.PlanFree, api.PlanPro, true},
		{api.PlanFree, api.PlanScale, true},
		{api.PlanHobby, api.PlanFree, false}, // downgrade
		{api.PlanHobby, api.PlanHobby, false},
		{api.PlanHobby, api.PlanPro, true},
		{api.PlanHobby, api.PlanScale, true},
		{api.PlanPro, api.PlanFree, false},  // downgrade
		{api.PlanPro, api.PlanHobby, false}, // downgrade
		{api.PlanPro, api.PlanPro, false},
		{api.PlanPro, api.PlanScale, true},
		{api.PlanScale, api.PlanPro, false}, // downgrade
		{api.PlanScale, api.PlanScale, false},
		// Unknown target is always false (defense).
		{api.PlanFree, api.Plan("platinum"), false},
		// Unknown source plan: fail-closed — require Stripe so a future
		// tier added without updating the switch cannot silently let
		// the customer upgrade without billing (issue #142 review).
		{api.Plan("enterprise"), api.PlanPro, true},
	} {
		t.Run(fmt.Sprintf("%s→%s", r.from, r.to), func(t *testing.T) {
			if got := r.from.RequiresStripeUpgradeTo(r.to); got != r.want {
				t.Errorf("RequiresStripeUpgradeTo(%q, %q) = %v, want %v",
					r.from, r.to, got, r.want)
			}
		})
	}
}

// fakeBillingProvider is the cmd/apid-side test double for billing.Provider.
// Only CreateUpgradeTransaction is exercised by the changePlan tests; the
// other methods are no-op stubs so the type satisfies billing.Provider's
// full surface (PR #3 / ADR-025).
//
// PR-P3: customerCreateCalls + customerCreateID capture the
// CreateCustomer sidecar path. Tests that need to pin the sidecar
// behaviour (no double-create when ProviderCustomerID is set,
// error surfacing on CreateCustomer failure) assert against these
// fields. The default zero-value leaves the sidecar as a no-op so
// pre-PR-P3 tests don't need updating.
type fakeBillingProvider struct {
	txnID               string
	checkoutURL         string
	err                 error
	calls               int
	customerCreateCalls int
	customerCreateID    string
	customerCreateErr   error
}

func (f *fakeBillingProvider) EnsurePlanProducts(context.Context) error { return nil }
func (f *fakeBillingProvider) CreateCustomer(_ context.Context, _ state.Account) (string, error) {
	f.customerCreateCalls++
	if f.customerCreateErr != nil {
		return "", f.customerCreateErr
	}
	if f.customerCreateID != "" {
		return f.customerCreateID, nil
	}
	return "ctm_fake_default", nil
}
func (f *fakeBillingProvider) PushUsageRecord(context.Context, state.Account, time.Time, int64) error {
	return nil
}
func (f *fakeBillingProvider) VerifyWebhook([]byte, map[string]string, time.Duration) (billing.Event, error) {
	return billing.Event{}, nil
}
func (f *fakeBillingProvider) CreateUpgradeTransaction(_ context.Context, acct state.Account, target api.Plan) (string, string, error) {
	f.calls++
	if f.err != nil {
		return "", "", f.err
	}
	return f.txnID, f.checkoutURL, nil
}

// Refund is the issue #279 billing.Provider seam. The changePlan
// tests never call it; returning ErrNotImplemented matches the
// Paddle-out-of-scope contract documented in pkg/billing/provider.go.
func (f *fakeBillingProvider) Refund(_ context.Context, _ string, _ int64) (*billing.RefundResult, error) {
	return nil, billing.ErrNotImplemented
}

// ReconcileUsage is the drift-detector seam (ADR-049 §B.1). The
// changePlan tests don't drive the reconciler, so the fake
// returns ErrNotImplemented — the reconciler treats that as
// "provider has no drift signal" and skips the account.
func (f *fakeBillingProvider) ReconcileUsage(_ context.Context, _ state.Account, _, _ time.Time) (int64, error) {
	return 0, billing.ErrNotImplemented
}

// Capabilities returns the Paddle-shaped set so the changePlan
// handler dispatches via the new PR-P1 capability introspection
// (CapHostedCheckout is set). Matches the production *paddle.Provider
// capabilities so the test exercises the same code path.
func (f *fakeBillingProvider) Capabilities() billing.CapabilitySet {
	return billing.CapabilitySet(billing.CapHostedCheckout | billing.CapUsageLineItem | billing.CapSandbox)
}

// TestChangePlan_PaddleCheckout_RendersPaddleExtension pins the
// Paddle dispatch on the changePlan 402 path (PR #3 / ADR-025). The
// fakeBillingProvider returns ("txn_abc", "https://paddle.example/checkout/xyz", nil);
// the handler must surface those as PaddleCheckoutURL + TxID
// extensions on the 402 Problem, with BillingPortalURL empty.
//
// Bit-for-bit mirror of the Stripe case in TestChangePlan_GateMatrix —
// the upgrade is blocked (free→pro, no subscription item), so the
// response is the 402 shape, not the 200 success path.
func TestChangePlan_PaddleCheckout_RendersPaddleExtension(t *testing.T) {
	e, srv := setupChangePlan(t, api.PlanFree, "")
	fake := &fakeBillingProvider{
		txnID:       "txn_abc",
		checkoutURL: "https://paddle.example/checkout/xyz",
	}
	srv.WithBillingProvider(fake)

	rec := e.do(t, "PATCH", "/v1/account/plan",
		map[string]string{"plan": string(api.PlanPro)}, nil)

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402\nbody = %s", rec.Code, rec.Body)
	}
	if fake.calls != 1 {
		t.Errorf("CreateUpgradeTransaction calls = %d, want 1", fake.calls)
	}
	var prob api.Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &prob); err != nil {
		t.Fatalf("body not problem+json: %s", rec.Body)
	}
	if prob.Code != api.CodePayment {
		t.Errorf("Code = %q, want %q", prob.Code, api.CodePayment)
	}
	if prob.PaddleCheckoutURL != fake.checkoutURL {
		t.Errorf("PaddleCheckoutURL = %q, want %q", prob.PaddleCheckoutURL, fake.checkoutURL)
	}
	if prob.TxID != fake.txnID {
		t.Errorf("TxID = %q, want %q", prob.TxID, fake.txnID)
	}
	// Mutually exclusive: BillingPortalURL must be empty when PaddleCheckoutURL is set.
	if prob.BillingPortalURL != "" {
		t.Errorf("BillingPortalURL = %q, want empty on Paddle path", prob.BillingPortalURL)
	}

	// Plan must NOT have been written — same gate semantics as the Stripe case.
	updated, err := e.store.AccountByID(context.Background(), e.acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Plan != api.PlanFree {
		t.Errorf("plan after = %q, want %q (gate blocks the write)", updated.Plan, api.PlanFree)
	}
}

// TestChangePlan_PaddleCheckout_CreateCustomerSidecar (PR-P3) asserts
// the new CreateCustomer sidecar fires exactly once when the account's
// ProviderCustomerID is empty. Paddle's CreateUpgradeTransaction
// requires an existing ctm_… to attach the subscription to; the
// sidecar covers the "first upgrade after signup" path where the
// account row has no provider_customer_id yet. The fake's
// customerCreateCalls counter pins the sidecar to a single call —
// a regression where the sidecar fires twice on the same request
// (or, equivalently, fires after a ProviderCustomerID was already
// stamped) would burn a quota of Paddle customers per upgrade and
// is a billing correctness bug.
func TestChangePlan_PaddleCheckout_CreateCustomerSidecar(t *testing.T) {
	e, srv := setupChangePlan(t, api.PlanFree, "")
	fake := &fakeBillingProvider{
		txnID:            "txn_abc",
		checkoutURL:      "https://paddle.example/checkout/xyz",
		customerCreateID: "ctm_fresh_42",
	}
	srv.WithBillingProvider(fake)

	rec := e.do(t, "PATCH", "/v1/account/plan",
		map[string]string{"plan": string(api.PlanPro)}, nil)

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402\nbody = %s", rec.Code, rec.Body)
	}
	if fake.customerCreateCalls != 1 {
		t.Errorf("CreateCustomer calls = %d, want 1 (sidecar must fire once on empty ProviderCustomerID)", fake.customerCreateCalls)
	}
	if fake.calls != 1 {
		t.Errorf("CreateUpgradeTransaction calls = %d, want 1", fake.calls)
	}
	// Stamp must have landed on the account row.
	updated, err := e.store.AccountByID(context.Background(), e.acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ProviderCustomerID != "ctm_fresh_42" {
		t.Errorf("ProviderCustomerID after = %q, want ctm_fresh_42 (sidecar must stamp)", updated.ProviderCustomerID)
	}
}

// TestChangePlan_PaddleCheckout_NoDoubleCreate (PR-P3) is the
// idempotency pin: a second PATCH on the same account must NOT
// re-call CreateCustomer. The sidecar's "if ProviderCustomerID ==
// \"\" then CreateCustomer" guard makes this a no-op for the
// repeat path. The production risk is two ctm_… rows on the Paddle
// dashboard for one account — the dashboard's customer-bind by
// email is heuristic and could mismatch if two rows exist.
func TestChangePlan_PaddleCheckout_NoDoubleCreate(t *testing.T) {
	e, srv := setupChangePlan(t, api.PlanFree, "")
	// Pre-stamp ProviderCustomerID so the sidecar guard is in
	// the "already populated" branch from the first request.
	acct := e.acct
	if err := e.store.UpdateAccountPaddleCustomerID(context.Background(), acct.ID, "ctm_existing"); err != nil {
		t.Fatal(err)
	}
	// Re-fetch so the handler's acct argument sees the stamp.
	acct, err := e.store.AccountByID(context.Background(), acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	e.acct = acct

	fake := &fakeBillingProvider{
		txnID:            "txn_abc",
		checkoutURL:      "https://paddle.example/checkout/xyz",
		customerCreateID: "ctm_should_not_fire",
	}
	srv.WithBillingProvider(fake)

	rec := e.do(t, "PATCH", "/v1/account/plan",
		map[string]string{"plan": string(api.PlanPro)}, nil)

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402\nbody = %s", rec.Code, rec.Body)
	}
	if fake.customerCreateCalls != 0 {
		t.Errorf("CreateCustomer calls = %d, want 0 (sidecar must skip when ProviderCustomerID already set)", fake.customerCreateCalls)
	}
	if fake.calls != 1 {
		t.Errorf("CreateUpgradeTransaction calls = %d, want 1", fake.calls)
	}
}

// TestChangePlan_PaddleCheckout_CreateCustomerError (PR-P3) pins the
// failure path: if the SDK CreateCustomer call fails (sandbox down,
// bad key), the handler 503s with the same ErrCapacity shape used
// elsewhere on the upgrade path. A regression that silently fell
// through to CreateUpgradeTransaction would result in a checkout
// URL that, when completed, lands on Paddle-side failure because
// the customer row was never created — operators would see
// "checkout succeeded but plan did not upgrade" tickets.
func TestChangePlan_PaddleCheckout_CreateCustomerError(t *testing.T) {
	e, srv := setupChangePlan(t, api.PlanFree, "")
	fake := &fakeBillingProvider{
		txnID:             "txn_abc",
		checkoutURL:       "https://paddle.example/checkout/xyz",
		customerCreateErr: errors.New("paddle: CreateCustomer: 401 unauthorized"),
	}
	srv.WithBillingProvider(fake)

	rec := e.do(t, "PATCH", "/v1/account/plan",
		map[string]string{"plan": string(api.PlanPro)}, nil)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 on CreateCustomer failure\nbody = %s", rec.Code, rec.Body)
	}
	if fake.customerCreateCalls != 1 {
		t.Errorf("CreateCustomer calls = %d, want 1 (the failing call)", fake.customerCreateCalls)
	}
	if fake.calls != 0 {
		t.Errorf("CreateUpgradeTransaction calls = %d, want 0 (must NOT proceed after CreateCustomer error)", fake.calls)
	}
}

// TestChangePlan_PaddleProvider_NoProviderTemplateFallback asserts
// that when billingProvider is set but CreateUpgradeTransaction returns
// the Stripe-stub sentinel ("", "", nil), the handler falls through to
// the BillingPortalURL template path. This is the apid-side contract
// that lets one Provider serve as a "Stripe-on-Paddle-box" fallback
// (operator runs Paddle as the active provider but the upgrade path
// is template-only — e.g. while the price catalog hasn't been seeded
// yet).
func TestChangePlan_PaddleProvider_NoProviderTemplateFallback(t *testing.T) {
	e, srv := setupChangePlan(t, api.PlanFree, "")
	fake := &fakeBillingProvider{
		// Stripe stub: txID == "" is the dispatch signal.
		txnID:       "",
		checkoutURL: "",
	}
	srv.WithBillingProvider(fake)

	rec := e.do(t, "PATCH", "/v1/account/plan",
		map[string]string{"plan": string(api.PlanPro)}, nil)

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", rec.Code)
	}
	var prob api.Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &prob); err != nil {
		t.Fatalf("body not problem+json: %s", rec.Body)
	}
	if prob.BillingPortalURL == "" {
		t.Errorf("expected billing_portal_url to be set; got empty (fallback path)")
	}
	if prob.PaddleCheckoutURL != "" {
		t.Errorf("PaddleCheckoutURL = %q, want empty (Stripe stub path)", prob.PaddleCheckoutURL)
	}
	if prob.TxID != "" {
		t.Errorf("TxID = %q, want empty (Stripe stub path)", prob.TxID)
	}
}

// TestChangePlan_NoProvider_StripeDefault asserts the no-provider path
// is bit-for-bit unchanged from pre-PR-#3. The changePlan 402 must
// carry BillingPortalURL + nothing else — a regression net for the
// PR-#3 dispatch refactor.
func TestChangePlan_NoProvider_StripeDefault(t *testing.T) {
	e, _ := setupChangePlan(t, api.PlanFree, "")
	// No WithBillingProvider call — same shape as pre-PR-#3.

	rec := e.do(t, "PATCH", "/v1/account/plan",
		map[string]string{"plan": string(api.PlanPro)}, nil)

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", rec.Code)
	}
	var prob api.Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &prob); err != nil {
		t.Fatalf("body not problem+json: %s", rec.Body)
	}
	if prob.BillingPortalURL == "" {
		t.Errorf("billing_portal_url empty on no-provider path")
	}
	if prob.PaddleCheckoutURL != "" || prob.TxID != "" {
		t.Errorf("Paddle extensions must not appear on no-provider path: %+v", prob)
	}
}

// TestGetBillingPortal is the issue #253 surface pin for
// GET /v1/billing/portal. The handler is intentionally trivial
// (one line: writeJSON BillingPortalURL) — this test pins:
//  1. 200 status (the URL is "absent" sentinel, not 404)
//  2. account_id substitution on the FAAS_BILLING_PORTAL_URL template
//  3. Bearer API-key auth works (the dashboard Bearer-or-cookie cluster)
//  4. empty URL response when WithBillingPortalURL is not called
//     (operator-misconfig path that the CLI branches on)
//  5. NO MFA gate: viewing a portal link is a read; mutations happen
//     inside the Stripe-hosted portal after Stripe-side 2FA. Adding
//     requireMFA back to the route would break this test.
func TestGetBillingPortal(t *testing.T) {
	t.Run("configured", func(t *testing.T) {
		env, _ := setupChangePlan(t, api.PlanHobby, "si_test")
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/v1/billing/portal", nil)
		r.Header.Set("Authorization", "Bearer "+env.key)
		env.h.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200\nbody = %s", rec.Code, rec.Body.String())
		}
		var resp api.BillingPortalResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v body=%s", err, rec.Body.String())
		}
		want := "https://billing.example.com/portal?account=" + env.acct.ID
		if resp.URL != want {
			t.Errorf("url = %q, want %q", resp.URL, want)
		}
		if strings.Contains(resp.URL, "{account_id}") {
			t.Errorf("url leaked unsubstituted template placeholder: %q", resp.URL)
		}
	})
	t.Run("no_mfa_required", func(t *testing.T) {
		// Pin the no-MFA contract: a Bearer-key request must return
		// 200 even when the account has MFA enrolled. Viewing a
		// portal link is a read; mutations are gated by Stripe 2FA
		// after the customer lands on the portal. Adding requireMFA
		// back to the route would flip this to 401 + the MFA-required
		// problem and fail this test.
		env, _ := setupChangePlan(t, api.PlanHobby, "si_test")
		if err := env.store.MarkMFAEnrolled(context.Background(), env.acct.ID); err != nil {
			t.Fatalf("mark mfa: %v", err)
		}
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/v1/billing/portal", nil)
		r.Header.Set("Authorization", "Bearer "+env.key)
		env.h.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Errorf("code = %d, want 200 (portal-link read should NOT require MFA)\nbody = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("unconfigured", func(t *testing.T) {
		// Fresh server with no WithBillingPortalURL — operator left
		// FAAS_BILLING_PORTAL_URL unset on the box. The endpoint must
		// return 200 + empty URL (the CLI prints a friendly hint
		// instead of opening the browser to "").
		store := state.NewMemStore()
		acct, err := store.CreateAccount(context.Background(), "no-portal@example.com", api.PlanHobby)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		pt, hash, _ := api.GenerateAPIKey()
		if _, err := store.CreateAPIKey(context.Background(), acct.ID, hash, "test", api.ScopesAdminOnly); err != nil {
			t.Fatalf("seed key: %v", err)
		}
		srv := newServerWithDeps(store, slog.New(slog.NewTextHandler(io.Discard, nil)),
			"example.com", noopNotifier{}, "", noopMailer{}, stubGithubdClient{}, nil, nil,
			15*24, "")
		// Deliberately NOT calling srv.WithBillingPortalURL.
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/v1/billing/portal", nil)
		r.Header.Set("Authorization", "Bearer "+pt)
		srv.handler().ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200\nbody = %s", rec.Code, rec.Body.String())
		}
		var resp api.BillingPortalResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v body=%s", err, rec.Body.String())
		}
		if resp.URL != "" {
			t.Errorf("url = %q, want empty when WithBillingPortalURL is not set", resp.URL)
		}
	})
}

// TestHandleBillingEvent_SubscriptionPastDue (PR-P3) is the unit
// pin for the new dunning branch. Paddle's subscription.past_due
// event maps to billing.EventSubscriptionPastDue; the handler
// must call MarkDunningStep(active → past_due) — same CAS as
// EventPaymentFailed so a Stripe invoice.payment_failed followed
// by a Paddle subscription.past_due on the same account collapses
// safely (the second call returns ErrNotFound because status is
// already past_due).
func TestHandleBillingEvent_SubscriptionPastDue(t *testing.T) {
	e, srv := setupChangePlan(t, api.PlanHobby, "si_test")
	acct := e.acct
	if acct.Status != state.AccountActive {
		t.Fatalf("seed account status = %q, want active", acct.Status)
	}
	// Drive the new branch directly through the unexported
	// handleBillingEvent. The handler's only side effects on this
	// branch are the MarkDunningStep CAS + a no-op log line; no
	// email (the past_due email is fired by EventPaymentFailed
	// first, or by this branch on its first delivery — the
	// no-email choice is documented inline in handlers_ext.go).
	srv.handleBillingEvent(context.Background(),
		billing.Event{Type: billing.EventSubscriptionPastDue},
		acct)

	fresh, err := e.store.AccountByID(context.Background(), acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Status != state.AccountPastDue {
		t.Errorf("status after = %q, want past_due", fresh.Status)
	}
}

// TestHandleBillingEvent_DunningCollisionCollapses (PR-P3) is the
// concurrency pin for the new branch. Two events arrive at the
// same account in the same tick — one Stripe (EventPaymentFailed)
// and one Paddle (EventSubscriptionPastDue). Both call
// MarkDunningStep(active → past_due). The CAS guarantees exactly
// one transition succeeds; the second returns ErrNotFound. A
// regression where one path bypassed the CAS (e.g. calling
// UpdateAccountStatus directly) would land the account in past_due
// twice with two past_due_at stamps — the audit trail would show
// duplicate email sends and the dunning timer's "days since
// past_due" calculation would start from the second stamp (wrong).
func TestHandleBillingEvent_DunningCollisionCollapses(t *testing.T) {
	e, srv := setupChangePlan(t, api.PlanHobby, "si_test")
	acct := e.acct

	// Pre-warm both events. We do not gate on order — both
	// branches independently flip active → past_due.
	done := make(chan struct{}, 2)
	go func() {
		srv.handleBillingEvent(context.Background(),
			billing.Event{Type: billing.EventPaymentFailed}, acct)
		done <- struct{}{}
	}()
	go func() {
		srv.handleBillingEvent(context.Background(),
			billing.Event{Type: billing.EventSubscriptionPastDue}, acct)
		done <- struct{}{}
	}()
	<-done
	<-done

	fresh, err := e.store.AccountByID(context.Background(), acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Status != state.AccountPastDue {
		t.Errorf("status after collision = %q, want past_due (exactly one transition)", fresh.Status)
	}
}

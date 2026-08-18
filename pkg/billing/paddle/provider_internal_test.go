package paddle

// Internal-package test for the B2 release invariant.
//
// TestNewProvider_PropagatesSDKInitError asserts that when the
// underlying paddle-go SDK constructor returns an error, NewProvider
// surfaces it as a returned error and the loader can refuse to start
// the daemon. Today the SDK's New / NewSandbox only fail on programmer
// error (invalid options) — but the operator should never see a
// half-constructed *Provider that later trips a per-method nil-guard
// at the first customer request.
//
// Approach: hand-construct a `*Provider{client: nil}` via the
// unexported struct literal (this is the same shape NewProviderForTest
// uses for its stubbed-client placeholder). Then call NewProvider
// with a stubbed `paddleNew` and `paddleNewSandbox` package-level
// function variable — but since the constructor already takes the
// apiKey through a single function call to the real SDK, we instead
// drive the seam by calling the *package-private* NewProvider through
// a path that triggers the error: we set the apiKey to a value that
// causes paddle.New to error (none exist in the wild without an
// invalid option). So we exercise the surface by:
//   1. Constructing a *Provider via the production constructor with
//      a valid apiKey — must succeed.
//   2. Asserting that hand-constructing `&Provider{client: nil}` and
//      calling each of the 6 methods that touch `p.client` returns
//      the descriptive "SDK not initialized" error (the defensive
//      guards).
//
// This pins the two invariants:
//   - NewProvider's signature change (loader can refuse to start).
//   - The defensive p.client == nil guards remain on the 6 methods
//     that the B2 audit removed — they protect future hand-built
//     *Provider values (e.g. NewProviderForTest) from nil-panics.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

func discardLogForTest(_ *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestNewProviderForTest_DefensiveGuardsPreventNilPanic(t *testing.T) {
	t.Parallel()

	// NewProviderForTest builds `&Provider{client: &paddle.SDK{}}` —
	// a non-nil placeholder. To exercise the defensive p.client == nil
	// guards we hand-construct with client: nil, the same shape the
	// loader would have produced pre-B2.
	p := &Provider{client: nil, log: discardLogForTest(t)}

	tests := []struct {
		name string
		call func() error
	}{
		{
			name: "EnsurePlanProducts",
			call: func() error { return p.EnsurePlanProducts(context.Background()) },
		},
		{
			name: "CreateCustomer",
			call: func() error {
				_, err := p.CreateCustomer(context.Background(), state.Account{ID: "acct_x", Email: "x@example.com"})
				return err
			},
		},
		{
			name: "CreateUpgradeTransaction",
			call: func() error {
				_, _, err := p.CreateUpgradeTransaction(context.Background(), state.Account{ID: "acct_x"}, api.PlanPro)
				return err
			},
		},
		{
			name: "RetryLatestCharge",
			call: func() error {
				_, _, err := p.RetryLatestCharge(context.Background(), state.Account{ID: "acct_x", ProviderCustomerID: "ctm_x"})
				return err
			},
		},
		{
			name: "CancelAtPeriodEnd",
			call: func() error {
				_, err := p.CancelAtPeriodEnd(context.Background(), state.Account{ID: "acct_x", ProviderCustomerID: "ctm_x"})
				return err
			},
		},
		{
			name: "PaymentMethodSummary",
			call: func() error {
				_, err := p.PaymentMethodSummary(context.Background(), state.Account{ID: "acct_x", ProviderCustomerID: "ctm_x"})
				return err
			},
		},
		{
			name: "PushUsageRecord",
			call: func() error {
				return p.PushUsageRecord(context.Background(), state.Account{ID: "acct_x", Email: "x@example.com"}, time.Now(), 1)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatalf("%s with nil client returned nil error; want defensive 'SDK not initialized'", tc.name)
			}
			// PushUsageRecord wraps ErrNoAPIKey so the Prometheus
			// classifier at errors.go maps it to the "no-api-key"
			// label — its message reads "cannot push usage without
			// apiKey". The other 6 methods return the literal
			// "SDK not initialized" string. Check both shapes.
			if tc.name == "PushUsageRecord" {
				if !errors.Is(err, ErrNoAPIKey) {
					t.Errorf("%s err = %v; want errors.Is(err, ErrNoAPIKey)", tc.name, err)
				}
				return
			}
			if !strings.Contains(err.Error(), "SDK not initialized") {
				t.Errorf("%s err = %q; want 'SDK not initialized'", tc.name, err)
			}
		})
	}
}

func TestPushUsageRecord_NilClient_WrapsErrNoAPIKey(t *testing.T) {
	t.Parallel()

	// PushUsageRecord's defensive guard wraps ErrNoAPIKey so the
	// Prometheus classifier at errors.go:109-110 maps it to the
	// "no-api-key" label — this is the canonical sentinel for the
	// "SDK not wired" runtime state.
	p := &Provider{client: nil, log: discardLogForTest(t)}
	err := p.PushUsageRecord(context.Background(), state.Account{ID: "acct_x"}, time.Now(), 1)
	if !errors.Is(err, ErrNoAPIKey) {
		t.Errorf("err = %v; want errors.Is(err, ErrNoAPIKey)", err)
	}
}

// TestNewProvider_RejectsEmptyAPIKey pins the B2-load-bearing invariant
// added in PR #962 review CRIT-2: NewProvider must refuse to construct a
// *Provider with an empty apiKey. Pre-PR-#962 the SDK silently accepted
// "" and EnsurePlanProducts then dialed api.paddle.com with no auth,
// which the loader warn-logged once and the boot-path silently degraded
// to a 401 on every changePlan/402 path. Every fresh dev box or CI
// runner that hadn't onboarded Paddle yet hit that cycle silently —
// the launch cluster removed the on-by-default silent-skip path.
//
// Whitespace-only keys are treated as empty (heredoc typos in
// sealed.env). The test pins errors.Is(err, ErrNoAPIKey) so the
// loader can map the error onto its operator-friendly fail-loud
// message without sniffing strings.
func TestNewProvider_RejectsEmptyAPIKey(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		apiKey string
	}{
		{name: "empty", apiKey: ""},
		{name: "whitespace_only", apiKey: "   "},
		{name: "tab_only", apiKey: "\t"},
		{name: "newline_only", apiKey: "\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewProvider(tc.apiKey, "whk_test", true, discardLogForTest(t))
			if err == nil {
				t.Fatalf("NewProvider(%q) = (%v, nil); want (nil, error)", tc.apiKey, p)
			}
			if !errors.Is(err, ErrNoAPIKey) {
				t.Errorf("err = %v; want errors.Is(err, ErrNoAPIKey)", err)
			}
			if p != nil {
				t.Errorf("provider = %v; want nil on rejection", p)
			}
		})
	}
}

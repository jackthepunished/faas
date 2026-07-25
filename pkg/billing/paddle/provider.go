package paddle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/PaddleHQ/paddle-go-sdk/v5"
	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/billing"
	"github.com/onebox-faas/faas/pkg/state"
)

// PlanPriceIDs + OveragePriceIDs together hold the price handles
// Paddle returned from EnsurePlanProducts. Keys are api.Plan values
// (free/hobby/pro/scale) so the meterd pusher can look up the
// overage line-item handle without re-stamping strings.
//
// The cache lives on the Client because it is constructed once at
// boot and read by every PushUsageRecord call. concurrent-safe via
// the same mutex — pattern matches pkg/billing/stripe's PlanPriceIDs.
type priceCatalog struct {
	mu            sync.RWMutex
	planMonthly   map[api.Plan]string // plan → pri_…
	planOverage   map[api.Plan]string // plan → pri_…
	planCustomers map[api.Plan]string // plan → pro_…
}

// Provider is the Paddle Billing v2 implementation of billing.Provider.
// All four interface methods map onto the paddle-go SDK's REST endpoints;
// provider-specific wire-format concerns (signature scheme, line-item
// shape, customer ID format) stay inside this package — apid and
// meterd see only billing.Event / state.Account / the Provider
// interface. ADR-025.
type Provider struct {
	apiKey        string
	webhookSecret string
	client        *paddle.SDK
	log           *slog.Logger
	catalog       *priceCatalog
	// flushFn is the test seam defaultFlushLocked is reached through.
	// nil → defaultFlushLocked (production). The seam matters: the PR
	// that introduced the stateless per-push shape (this PR) needed a
	// way to assert "no SDK POST fired" without standing up a real
	// *paddle.SDK, and counter stubs are the cheapest way to do that.
	//
	// Kept on the Provider (not on a constructor-level opts struct) so
	// the test code can swap flushFn on a single constructed instance
	// instead of re-constructing the whole provider per test case.
	flushFn FlushFn
	// dedupe is the state-store-backed cross-process gate consulted
	// before each Paddle CreateTransaction POST and stamped after.
	// nil → within-process dedupe only (apid's path; apid never pushes
	// overage, so the only writer is meterd's). meterd wires it via
	// NewProviderWithDedupe so a crash between POST and stamp cannot
	// cause a second POST on the next process boot.
	dedupe PaddleOverageDedupe
	// createUpgradeTxnFn is the seam CreateUpgradeTransaction delegates
	// to. Tests substitute a counter/recorder stub so they can assert
	// the SDK request shape (price handle, CustomData, Idempotency-Key
	// tag) without standing up a full *paddle.SDK. nil → the default
	// production body (defaultCreateUpgradeTxn).
	createUpgradeTxnFn CreateUpgradeTxnFn
	now                func() time.Time
}

// NewProvider wires the Paddle v5 SDK. sandbox=true →
// api.sandbox.paddle.com (operator's free sandbox); false →
// api.paddle.com (production).
//
// The SDK is initialized with a custom HTTP client whose Transport
// is wrapped by NewIdempotencyRT — every Writes request the SDK
// emits (CreateTransaction, etc.) flows through the wrapper, which
// copies X-Transit-Id (set by the SDK from paddle.ContextWithTransitID)
// as Idempotency-Key on POST /transactions. See transport.go for the
// full design rationale. The paddle-go-sdk/v5@v5.2.0 SDK exposes
// paddle.WithClient(c client.HTTPDoer) for this — the *http.Client
// we pass satisfies that interface via its Do method.
//
// Catalog + time hooks are initialized lazily so tests can construct
// without live configuration. EnsurePlanProducts must be called
// before PushUsageRecord / CreateCustomer in production; both fail
// fast with a descriptive error if the catalog is empty.
func NewProvider(apiKey, webhookSecret string, sandbox bool, log *slog.Logger) *Provider {
	if log == nil {
		log = slog.Default()
	}
	var client *paddle.SDK
	var err error
	httpClient := &http.Client{Transport: NewIdempotencyRT(http.DefaultTransport)}
	if sandbox {
		client, err = paddle.NewSandbox(apiKey, paddle.WithClient(httpClient))
	} else {
		client, err = paddle.New(apiKey, paddle.WithClient(httpClient))
	}
	if err != nil {
		// NewSandbox / New only fail on programmer error (invalid
		// options); surface loudly so the daemon doesn't bind silently.
		log.Error("paddle: SDK init failed", "err", err, "sandbox", sandbox)
	}
	return &Provider{
		apiKey:        apiKey,
		webhookSecret: webhookSecret,
		client:        client,
		log:           log,
		catalog:       &priceCatalog{planMonthly: map[api.Plan]string{}, planOverage: map[api.Plan]string{}, planCustomers: map[api.Plan]string{}},
		now:           time.Now,
	}
}

// NewProviderWithDedupe is the meterd-side constructor. Same as
// NewProvider but with the state-store-backed cross-process dedupe
// wired so a meterd crash between the Paddle CreateTransaction POST
// and the in-process `acc.flushed` stamp cannot cause a second POST
// on the next process boot for the same (account, month). apid's path
// uses NewProvider (no ingress from apid writes to the overage
// accumulator; the only accumulator is meterd's).
//
// Keeping this as a separate constructor (rather than a WithDedupe
// option) avoids touching every existing test call site that uses
// NewProvider — the loader is the only caller that needs the dedupe.
func NewProviderWithDedupe(apiKey string, sandbox bool, log *slog.Logger, dedupe PaddleOverageDedupe) *Provider {
	p := NewProvider(apiKey, "", sandbox, log)
	p.dedupe = dedupe
	return p
}

// Compile-time conformance to billing.Provider. Adding a method to the
// interface is a build error here — mirrors pkg/billing/stripe.
var _ billing.Provider = (*Provider)(nil)

// ---- billing.Provider surface ----

// EnsurePlanProducts: idempotent boot-time setup. Lists products +
// prices; for any missing plan, creates the product, a monthly
// recurring price, and a flat-rate overage line-item price. Matches
// on name prefix `faas-plan-<plan>` so re-running on boot is a
// no-op. Maps onto Paddle's list-then-create pattern (Stripe uses
// Nicknames; Paddle has no equivalent, so we use Name).
//
// Idempotency: redelivered boot on the same platform hits the
// `Status: active` filter on ListProducts, finds the existing
// products/prices, and skips the POST. No merchant-side flag.
func (p *Provider) EnsurePlanProducts(ctx context.Context) error {
	if p.client == nil {
		return fmt.Errorf("paddle: SDK not initialized (apiKey=%q)", redactAPIKey(p.apiKey))
	}
	if err := p.ensurePlansAndPrices(ctx); err != nil {
		return fmt.Errorf("paddle: ensure plans: %w", err)
	}
	p.log.Info("paddle: EnsurePlanProducts complete", "monthly", p.snapshotPlans(), "overage", p.snapshotOverage())
	return nil
}

func (p *Provider) ensurePlansAndPrices(ctx context.Context) error {
	// Implementation lives in products.go. Kept as a thin forward so
	// provider.go stays a Provider-surface file.
	return p.ensureProducts(ctx)
}

// PushUsageRecord: per-push stateless overage flush. Paddle Billing v2
// has no equivalent of Stripe's metered subscription_item — the shape
// is a single Transactions POST with a price_id (the overage line item)
// and quantity 1. The meterd pusher loop sums that month's mb_seconds
// from usage_minutes rows on every tick and calls this with the sum.
//
// The pre-SDK guards (no apiKey, negative qty, missing overage price)
// return sentinels from usage.go so the classifier at errors.go can
// map them to stable Prometheus labels. Adding a new pre-SDK failure
// mode requires adding a sentinel + a label — the closed label set is
// the dashboard's panel surface, so the change is deliberate.
//
// Concurrency: meter (cmd/meterd) calls this from a single loop
// goroutine; apid's webhook handler does not. The meter's loop
// holds a single contract: at most one outstanding call per
// (acct.ID, month). Tests pin that contract.
//
// Idempotency: each call carries an Idempotency-Key header derived
// from (acct.ID, month) via the NewIdempotencyRT wrapper installed
// at NewProvider. The cross-process dedupe gate (HasPaddleOverageMonth
// / RecordPaddleOverageMonth) collapses on the same shape so a
// redelivered month — across a meterd restart or a stripe-vs-paddle
// test path — is a no-op before the SDK is invoked. Paddle's API
// server may not honor Idempotency-Key today (SDK team is working
// on native support); the header presence is observable on the wire
// for ops debugging and is forward-compat.
func (p *Provider) PushUsageRecord(ctx context.Context, acct state.Account, hour time.Time, mbSeconds int64) error {
	if p.client == nil {
		return fmt.Errorf("%w (account %s)", ErrNoAPIKey, acct.ID)
	}
	if acct.Email == "" {
		return errors.New("paddle: PushUsageRecord requires acct.Email")
	}
	if mbSeconds < 0 {
		return fmt.Errorf("paddle: PushUsageRecord: %w (account %s, qty %d)", ErrNegativeMBSeconds, acct.ID, mbSeconds)
	}
	if mbSeconds == 0 {
		// Defensive: meterd pusher loop filters 0-sum pushes before
		// calling us, but the guard is here for future callers (tests,
		// other ingress paths). flushOverageLocked has the same guard so
		// both surfaces are idempotent on 0.
		return nil
	}
	return p.flushOverageLocked(ctx, acct, hour, mbSeconds)
}

// VerifyWebhook: HMAC-SHA256 over "<unix>:<body>" with the
// Paddle-Signature header's h1= value. Constant-time compare via
// crypto/hmac.Equal (same pattern as pkg/billing/stripe/webhook.go
// but with Paddle's `: ` separator instead of Stripe's `.`).
//
// Header format: `ts=<unix>;h1=<hex-sha256>`. Captured by regex;
// the timestamp is unix-seconds (matching Stripe's t= value for
// interface symmetry).
//
// Returns billing.Event with normalized EventType. mapping in
// mapPaddleEventType; unknown events render as EventUnknown so
// apid's switch falls through to a 200 no-op (Paddle retries on
// 5xx; we 200 unknown types so it doesn't retry forever).
func (p *Provider) VerifyWebhook(payload []byte, headers map[string]string, tolerance time.Duration) (billing.Event, error) {
	if p.webhookSecret == "" {
		return billing.Event{}, fmt.Errorf("paddle: %w: empty webhook secret", billing.ErrBadSignature)
	}
	sigHeader := headers["Paddle-Signature"]
	if sigHeader == "" {
		sigHeader = headers["paddle-signature"]
	}
	if sigHeader == "" {
		return billing.Event{}, fmt.Errorf("paddle: %w: missing Paddle-Signature header", billing.ErrBadSignature)
	}
	if err := verifyPaddleSignature(payload, sigHeader, p.webhookSecret, tolerance); err != nil {
		return billing.Event{}, err
	}
	return parsePaddleEvent(payload)
}

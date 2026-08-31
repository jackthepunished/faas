// Package polar implements the Polar merchant-of-record billing provider.
// It deliberately uses Polar's documented REST API directly: the official Go
// SDK was archived, while the HTTP contract is small and stable enough for a
// focused provider facade.
package polar

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/billing"
	"github.com/onebox-faas/faas/pkg/state"
)

const (
	productionBaseURL  = "https://api.polar.sh"
	sandboxBaseURL     = "https://sandbox-api.polar.sh"
	maxErrorBody       = 64 << 10
	maxRequestAttempts = 3
	retryBaseDelay     = 100 * time.Millisecond
)

// usageDedupe is the existing hourly push table exposed by state.Store. The
// table has a historical Stripe name, but its (account, hour) semantics are
// provider-neutral and exactly match Polar's event-push boundary.
type usageDedupe interface {
	HasStripePushHour(context.Context, string, time.Time) (bool, error)
	RecordStripePushHour(context.Context, string, time.Time) error
}

// Provider is the Polar implementation of billing.Provider.
type Provider struct {
	apiKey        string
	webhookSecret string
	baseURL       string
	usageEvent    string
	meterID       string
	products      map[api.Plan]string
	successURL    string
	returnURL     string
	client        *http.Client
	log           *slog.Logger
	dedupe        usageDedupe
	webhookTol    time.Duration
	now           func() time.Time
}

var _ billing.Provider = (*Provider)(nil)
var _ billing.Classifier = (*Provider)(nil)

// PolarCapabilities returns the static capabilities of this provider.
func PolarCapabilities() billing.CapabilitySet {
	return billing.CapabilitySet(
		billing.CapHostedCheckout |
			billing.CapRefund |
			billing.CapUsageMetered |
			billing.CapSandbox,
	)
}

// NewProvider constructs a Polar provider for apid. The API key is required
// for both apid and meterd because checkout/customer setup and usage ingestion
// are authenticated API operations.
func NewProvider(cfg Config, log *slog.Logger) (*Provider, error) {
	return newProvider(cfg, log, nil)
}

// NewProviderWithDedupe constructs the meterd-side provider with the durable
// hourly dedupe gate supplied by state.Store.
func NewProviderWithDedupe(cfg Config, log *slog.Logger, dedupe usageDedupe) (*Provider, error) {
	return newProvider(cfg, log, dedupe)
}

func newProvider(cfg Config, log *slog.Logger, dedupe usageDedupe) (*Provider, error) {
	cfg.Defaults()
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, fmt.Errorf("polar: %w (set FAAS_POLAR_ACCESS_TOKEN)", ErrNoAPIKey)
	}
	if log == nil {
		log = slog.Default()
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = productionBaseURL
		if cfg.Sandbox {
			base = sandboxBaseURL
		}
	}
	return &Provider{
		apiKey:        cfg.APIKey,
		webhookSecret: cfg.WebhookSecret,
		baseURL:       base,
		usageEvent:    cfg.UsageEventName,
		meterID:       cfg.MeterID,
		products: map[api.Plan]string{
			api.PlanHobby: cfg.HobbyProductID,
			api.PlanPro:   cfg.ProProductID,
			api.PlanScale: cfg.ScaleProductID,
		},
		successURL: cfg.SuccessURL,
		returnURL:  cfg.ReturnURL,
		client:     &http.Client{Timeout: 20 * time.Second},
		log:        log,
		dedupe:     dedupe,
		webhookTol: time.Duration(cfg.ToleranceSeconds) * time.Second,
		now:        time.Now,
	}, nil
}

// WebhookTolerance returns the configured Standard Webhooks timestamp window.
func (p *Provider) WebhookTolerance() time.Duration {
	if p == nil || p.webhookTol <= 0 {
		return 5 * time.Minute
	}
	return p.webhookTol
}

// SetWebhookTolerance is kept parallel with the Paddle provider so the loader
// can configure either provider through the same optional surface.
func (p *Provider) SetWebhookTolerance(d time.Duration) {
	if p == nil {
		return
	}
	if d <= 0 {
		p.webhookTol = 0
		return
	}
	p.webhookTol = d
}

func (p *Provider) Capabilities() billing.CapabilitySet {
	caps := PolarCapabilities()
	if p != nil && strings.TrimSpace(p.meterID) != "" {
		caps |= billing.CapabilitySet(billing.CapUsageReconcile)
	}
	return caps
}

// ClassifyPushError satisfies billing.Classifier while keeping Polar's
// provider-specific error taxonomy in errors.go.
func (p *Provider) ClassifyPushError(err error) string {
	return ClassifyPushError(err)
}

// EnsurePlanProducts validates the operator-owned Polar catalog. Unlike
// Paddle/Stripe, creating a product here would be unsafe: the recurring
// product must already contain the metered price and meter configured in the
// Polar dashboard. The IDs are therefore explicit deployment configuration.
func (p *Provider) EnsurePlanProducts(ctx context.Context) error {
	if p == nil {
		return errors.New("polar: provider is nil")
	}
	for _, plan := range []api.Plan{api.PlanHobby, api.PlanPro, api.PlanScale} {
		if strings.TrimSpace(p.products[plan]) == "" {
			return fmt.Errorf("polar: product id missing for plan=%s", plan)
		}
	}
	if strings.TrimSpace(p.usageEvent) == "" {
		return errors.New("polar: usage event name is empty")
	}
	if err := p.validateCatalog(ctx); err != nil {
		return err
	}
	return nil
}

// validateCatalog confirms that the configured dashboard-owned resources
// exist in the selected Polar environment. The provider intentionally does
// not create or mutate catalog resources, but accepting a typo here would
// otherwise defer a broken deployment to the first customer checkout.
func (p *Provider) validateCatalog(ctx context.Context) error {
	for plan, productID := range p.products {
		var product struct {
			ID string `json:"id"`
		}
		path := "/v1/products/" + url.PathEscape(productID)
		if err := p.doJSON(ctx, http.MethodGet, path, nil, &product, ""); err != nil {
			return fmt.Errorf("polar: validate %s product %q: %w", plan, productID, err)
		}
		if product.ID != "" && product.ID != productID {
			return fmt.Errorf("polar: validate %s product %q returned mismatched id %q", plan, productID, product.ID)
		}
	}
	if strings.TrimSpace(p.meterID) != "" {
		var meter struct {
			ID string `json:"id"`
		}
		path := "/v1/meters/" + url.PathEscape(p.meterID)
		if err := p.doJSON(ctx, http.MethodGet, path, nil, &meter, ""); err != nil {
			return fmt.Errorf("polar: validate meter %q: %w", p.meterID, err)
		}
		if meter.ID != "" && meter.ID != p.meterID {
			return fmt.Errorf("polar: validate meter %q returned mismatched id %q", p.meterID, meter.ID)
		}
	}
	return nil
}

type customerResponse struct {
	ID         string `json:"id"`
	ExternalID string `json:"external_id"`
}

// CreateCustomer gets the existing Polar customer by external ID before
// creating one. This makes retries safe even though the customer-create API
// does not expose a provider-specific idempotency key in its public contract.
func (p *Provider) CreateCustomer(ctx context.Context, acct state.Account) (string, error) {
	if acct.ID == "" {
		return "", errors.New("polar: CreateCustomer requires account ID")
	}
	if strings.TrimSpace(acct.Email) == "" {
		return "", errors.New("polar: CreateCustomer requires acct.Email")
	}
	if acct.ProviderCustomerID != "" {
		return acct.ProviderCustomerID, nil
	}
	var existing customerResponse
	err := p.doJSON(ctx, http.MethodGet, "/v1/customers/external/"+url.PathEscape(acct.ID), nil, &existing, "")
	if err == nil && existing.ID != "" {
		return existing.ID, nil
	}
	if err != nil && !hasStatus(err, http.StatusNotFound) {
		return "", fmt.Errorf("polar: get customer account=%s: %w", acct.ID, err)
	}
	body := map[string]any{
		"email":       acct.Email,
		"external_id": acct.ID,
		"metadata": map[string]any{
			"faas_account_id": acct.ID,
		},
	}
	var created customerResponse
	if err := p.doJSON(ctx, http.MethodPost, "/v1/customers", body, &created, "faas-customer-"+acct.ID); err != nil {
		// A concurrent creator can win between GET and POST. Re-read the
		// external-ID endpoint before surfacing the create error.
		if getErr := p.doJSON(ctx, http.MethodGet, "/v1/customers/external/"+url.PathEscape(acct.ID), nil, &existing, ""); getErr == nil && existing.ID != "" {
			return existing.ID, nil
		}
		return "", fmt.Errorf("polar: create customer account=%s: %w", acct.ID, err)
	}
	if created.ID == "" {
		return "", fmt.Errorf("polar: create customer account=%s returned empty ID", acct.ID)
	}
	p.log.Info("polar: customer created", "account", acct.ID, "customer_id", created.ID)
	return created.ID, nil
}

type usageEvent struct {
	ExternalID         string         `json:"external_id,omitempty"`
	Name               string         `json:"name"`
	ExternalCustomerID string         `json:"external_customer_id"`
	Metadata           map[string]any `json:"metadata"`
	Timestamp          string         `json:"timestamp,omitempty"`
}

type ingestRequest struct {
	Events []usageEvent `json:"events"`
}

type ingestResponse struct {
	Inserted   int `json:"inserted"`
	Duplicates int `json:"duplicates"`
}

// PushUsageRecord sends one immutable Polar event for the meterd window.
// Polar meters sum metadata.gb_ram_hours, while mb_seconds remains in the
// event for exact local reconciliation and operator auditability.
func (p *Provider) PushUsageRecord(ctx context.Context, acct state.Account, hour time.Time, mbSeconds int64) error {
	if mbSeconds < 0 {
		return fmt.Errorf("polar: %w (account=%s)", ErrNegativeMBSeconds, acct.ID)
	}
	if mbSeconds == 0 || acct.ProviderCustomerID == "" {
		return nil
	}
	window := hour.UTC().Truncate(time.Hour)
	if p.dedupe != nil {
		dup, err := p.dedupe.HasStripePushHour(ctx, acct.ID, window)
		if err != nil {
			return fmt.Errorf("polar: usage dedupe check account=%s hour=%s: %w", acct.ID, window.Format(time.RFC3339), err)
		}
		if dup {
			return nil
		}
	}
	quantity := float64(mbSeconds) / float64(billing.SecondsPerGBHour)
	body := ingestRequest{Events: []usageEvent{{
		ExternalID:         fmt.Sprintf("faas-usage-%s-%s", acct.ID, window.Format(time.RFC3339)),
		Name:               p.usageEvent,
		ExternalCustomerID: acct.ID,
		Metadata: map[string]any{
			"faas_account_id":      acct.ID,
			"provider_customer_id": acct.ProviderCustomerID,
			"window_start":         window.Format(time.RFC3339),
			"mb_seconds":           mbSeconds,
			"gb_ram_hours":         quantity,
		},
		Timestamp: window.Format(time.RFC3339),
	}}}
	var result ingestResponse
	idem := fmt.Sprintf("faas-usage-%s-%s", acct.ID, window.Format(time.RFC3339))
	if err := p.doJSON(ctx, http.MethodPost, "/v1/events/ingest", body, &result, idem); err != nil {
		return fmt.Errorf("polar: ingest usage account=%s hour=%s: %w", acct.ID, window.Format(time.RFC3339), err)
	}
	if result.Inserted == 0 && result.Duplicates == 0 {
		return fmt.Errorf("polar: ingest usage account=%s hour=%s returned no inserted or duplicate events", acct.ID, window.Format(time.RFC3339))
	}
	if p.dedupe != nil {
		if err := p.dedupe.RecordStripePushHour(ctx, acct.ID, window); err != nil {
			return fmt.Errorf("polar: usage dedupe record account=%s hour=%s: %w", acct.ID, window.Format(time.RFC3339), err)
		}
	}
	return nil
}

type checkoutResponse struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// CreateUpgradeTransaction creates a hosted Polar checkout for one recurring
// product. The external customer ID is included even when the local row has a
// Polar UUID, so webhook reconciliation remains tied to the FaaS account.
func (p *Provider) CreateUpgradeTransaction(ctx context.Context, acct state.Account, targetPlan api.Plan) (string, string, error) {
	if !targetPlan.Valid() || targetPlan == api.PlanFree {
		return "", "", fmt.Errorf("polar: invalid paid target plan=%q", targetPlan)
	}
	productID := p.products[targetPlan]
	if productID == "" {
		return "", "", fmt.Errorf("polar: product id missing for plan=%s; configure Polar catalog IDs", targetPlan)
	}
	body := map[string]any{
		"products":             []string{productID},
		"external_customer_id": acct.ID,
		"customer_email":       acct.Email,
		"metadata": map[string]any{
			"faas_account_id": acct.ID,
			"target_plan":     string(targetPlan),
		},
	}
	if acct.ProviderCustomerID != "" {
		if _, err := uuid.Parse(acct.ProviderCustomerID); err == nil {
			body["customer_id"] = acct.ProviderCustomerID
		}
	}
	if p.successURL != "" {
		body["success_url"] = p.successURL
	}
	if p.returnURL != "" {
		body["return_url"] = p.returnURL
	}
	var checkout checkoutResponse
	idem := fmt.Sprintf("faas-upgrade-%s-%s", acct.ID, targetPlan)
	if err := p.doJSON(ctx, http.MethodPost, "/v1/checkouts", body, &checkout, idem); err != nil {
		return "", "", fmt.Errorf("polar: create checkout account=%s plan=%s: %w", acct.ID, targetPlan, err)
	}
	if checkout.ID == "" || checkout.URL == "" {
		return "", "", fmt.Errorf("polar: create checkout returned empty id or url for account=%s plan=%s", acct.ID, targetPlan)
	}
	return checkout.ID, checkout.URL, nil
}

type customerSessionResponse struct {
	PortalURL string `json:"customer_portal_url"`
}

// CreateCustomerPortalSession returns a short-lived authenticated Polar
// customer portal URL. Polar identifies the customer by the same external
// account ID used by checkout and usage ingestion.
func (p *Provider) CreateCustomerPortalSession(ctx context.Context, acct state.Account, returnURL string) (string, error) {
	if acct.ID == "" {
		return "", errors.New("polar: customer portal requires account ID")
	}
	body := map[string]any{"external_customer_id": acct.ID}
	if returnURL != "" {
		body["return_url"] = returnURL
	}
	var session customerSessionResponse
	if err := p.doJSON(ctx, http.MethodPost, "/v1/customer-sessions", body, &session, ""); err != nil {
		return "", fmt.Errorf("polar: create customer portal session account=%s: %w", acct.ID, err)
	}
	if session.PortalURL == "" {
		return "", fmt.Errorf("polar: create customer portal session account=%s returned empty URL", acct.ID)
	}
	return session.PortalURL, nil
}

type quantitiesResponse struct {
	Total float64 `json:"total"`
}

// ReconcileUsage reads the configured Polar meter's total for the account and
// converts the gb_ram_hours meter quantity back to the exact local
// mb_seconds unit used by the drift detector. Polar's API exposes quantities
// as a number, so the conversion rounds only at the final integer boundary.
func (p *Provider) ReconcileUsage(ctx context.Context, acct state.Account, start, end time.Time) (int64, error) {
	if strings.TrimSpace(p.meterID) == "" {
		return 0, billing.ErrNotImplemented
	}
	if acct.ID == "" {
		return 0, errors.New("polar: usage reconciliation requires account ID")
	}
	if start.IsZero() || end.IsZero() || !start.Before(end) {
		return 0, errors.New("polar: usage reconciliation requires start before end")
	}
	query := url.Values{}
	query.Set("start_timestamp", start.UTC().Format(time.RFC3339Nano))
	query.Set("end_timestamp", end.UTC().Format(time.RFC3339Nano))
	query.Set("interval", "hour")
	query.Set("timezone", "UTC")
	query.Set("external_customer_id", acct.ID)
	var quantities quantitiesResponse
	path := "/v1/meters/" + url.PathEscape(p.meterID) + "/quantities?" + query.Encode()
	if err := p.doJSON(ctx, http.MethodGet, path, nil, &quantities, ""); err != nil {
		return 0, fmt.Errorf("polar: reconcile usage account=%s: %w", acct.ID, err)
	}
	if math.IsNaN(quantities.Total) || math.IsInf(quantities.Total, 0) || quantities.Total < 0 {
		return 0, fmt.Errorf("polar: reconcile usage account=%s returned invalid total %v", acct.ID, quantities.Total)
	}
	mbSeconds := quantities.Total * float64(billing.SecondsPerGBHour)
	if mbSeconds > float64(math.MaxInt64) {
		return 0, fmt.Errorf("polar: reconcile usage account=%s total overflows int64", acct.ID)
	}
	return int64(math.Round(mbSeconds)), nil
}

type refundResponse struct {
	ID       string `json:"id"`
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
	Status   string `json:"status"`
}

// Refund maps the billing interface's charge ID to Polar's order ID.
func (p *Provider) Refund(ctx context.Context, chargeID string, amountCents int64) (*billing.RefundResult, error) {
	if chargeID == "" {
		return nil, errors.New("polar: refund requires order ID")
	}
	if amountCents <= 0 {
		return nil, errors.New("polar: refund amount must be positive cents")
	}
	body := map[string]any{
		"order_id": chargeID,
		"reason":   "customer_request",
		"amount":   amountCents,
	}
	var refund refundResponse
	if err := p.doJSON(ctx, http.MethodPost, "/v1/refunds", body, &refund, fmt.Sprintf("faas-refund-%s-%d", chargeID, amountCents)); err != nil {
		return nil, fmt.Errorf("polar: create refund order=%s: %w", chargeID, err)
	}
	if refund.ID == "" {
		return nil, fmt.Errorf("polar: refund order=%s returned empty ID", chargeID)
	}
	if refund.Amount <= 0 {
		refund.Amount = amountCents
	}
	return &billing.RefundResult{
		ProviderRefundID: refund.ID,
		ChargeID:         chargeID,
		AmountCents:      refund.Amount,
		Currency:         refund.Currency,
		Status:           refund.Status,
	}, nil
}

// RequestInvoicePDF starts Polar's asynchronous invoice generation. The
// follow-up order.updated webhook carries is_invoice_generated=true and
// updates the persisted invoice projection.
func (p *Provider) RequestInvoicePDF(ctx context.Context, orderID string) error {
	if strings.TrimSpace(orderID) == "" {
		return errors.New("polar: invoice PDF requires order ID")
	}
	path := "/v1/orders/" + url.PathEscape(orderID) + "/invoice"
	if err := p.doJSON(ctx, http.MethodPost, path, nil, nil, "faas-invoice-pdf-"+orderID); err != nil {
		// Polar may report that the invoice has already been generated while
		// the delivery carrying is_invoice_generated=true is still in flight.
		if hasStatus(err, http.StatusConflict) {
			return nil
		}
		return fmt.Errorf("polar: request invoice PDF order=%s: %w", orderID, err)
	}
	return nil
}

// RetryLatestCharge is left unsupported because Polar's merchant API exposes
// orders but does not expose a direct “retry this saved payment” operation.
// Customers should use the Polar customer portal to update their payment
// method; the existing /v1/billing/retry endpoint then returns a truthful 501.
func (p *Provider) RetryLatestCharge(context.Context, state.Account) (string, string, error) {
	return "", "", billing.ErrNotImplemented
}

// subscriptionResponse is the small subset returned by Polar's subscription
// update endpoint that the account API needs.
type subscriptionResponse struct {
	ID                string    `json:"id"`
	CurrentPeriodEnd  time.Time `json:"current_period_end"`
	CancelAtPeriodEnd bool      `json:"cancel_at_period_end"`
}

// CancelAtPeriodEnd calls Polar's documented PATCH subscription operation.
// StripeSubscriptionItem is a historical column name used for the provider
// subscription ID by the shared account state contract.
func (p *Provider) CancelAtPeriodEnd(ctx context.Context, acct state.Account) (time.Time, error) {
	if acct.StripeSubscriptionItem == "" {
		return time.Time{}, fmt.Errorf("%w (account %s, no subscription)", billing.ErrAlreadyCancelled, acct.ID)
	}
	body := map[string]any{"cancel_at_period_end": true}
	var sub subscriptionResponse
	path := "/v1/subscriptions/" + url.PathEscape(acct.StripeSubscriptionItem)
	if err := p.doJSON(ctx, http.MethodPatch, path, body, &sub, "faas-cancel-"+acct.StripeSubscriptionItem); err != nil {
		return time.Time{}, fmt.Errorf("polar: cancel subscription account=%s: %w", acct.ID, err)
	}
	if sub.CurrentPeriodEnd.IsZero() {
		return time.Time{}, fmt.Errorf("polar: cancel subscription account=%s returned empty current_period_end", acct.ID)
	}
	return sub.CurrentPeriodEnd, nil
}

// PaymentMethodSummary intentionally returns the zero value. Polar keeps
// payment-method management in the customer portal and its merchant customer
// response exposes an opaque default_payment_method_id rather than a stable
// card brand/last4/expiry contract. Returning zero preserves the interface's
// no-card sentinel without inventing card data.
func (p *Provider) PaymentMethodSummary(context.Context, state.Account) (billing.PaymentMethod, error) {
	return billing.PaymentMethod{}, nil
}

func (p *Provider) doJSON(ctx context.Context, method, path string, in, out any, idem string) error {
	if p == nil {
		return errors.New("polar: provider is nil")
	}
	if strings.TrimSpace(p.apiKey) == "" {
		return ErrNoAPIKey
	}
	var encoded []byte
	if in != nil {
		var err error
		encoded, err = json.Marshal(in)
		if err != nil {
			return fmt.Errorf("polar: encode %s %s: %w", method, path, err)
		}
	}
	base := strings.TrimRight(p.baseURL, "/")
	client := p.client
	if client == nil {
		client = http.DefaultClient
	}
	// Only replay writes that carry an idempotency key. Customer-session
	// creation intentionally has no key in Polar's API and must not be
	// duplicated by a transport retry; GET/HEAD are safe without one.
	retryable := idem != "" || method == http.MethodGet || method == http.MethodHead
	for attempt := 1; attempt <= maxRequestAttempts; attempt++ {
		var body io.Reader
		if encoded != nil {
			body = bytes.NewReader(encoded)
		}
		req, err := http.NewRequestWithContext(ctx, method, base+path, body)
		if err != nil {
			return fmt.Errorf("polar: build %s %s: %w", method, path, err)
		}
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
		req.Header.Set("Accept", "application/json")
		if in != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if idem != "" {
			req.Header.Set("Idempotency-Key", idem)
		}
		resp, err := client.Do(req)
		if err != nil {
			if !retryable || attempt == maxRequestAttempts || ctx.Err() != nil {
				return err
			}
			if err := waitPolarRetry(ctx, retryBaseDelayFor(attempt, "")); err != nil {
				return err
			}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			errorBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
			_ = resp.Body.Close()
			apiErr := &APIError{Status: resp.StatusCode, Body: string(errorBody)}
			if !retryable || !retryablePolarStatus(resp.StatusCode) || attempt == maxRequestAttempts {
				return apiErr
			}
			if err := waitPolarRetry(ctx, retryBaseDelayFor(attempt, resp.Header.Get("Retry-After"))); err != nil {
				return err
			}
			continue
		}
		if out == nil || resp.StatusCode == http.StatusNoContent {
			_ = resp.Body.Close()
			return nil
		}
		err = json.NewDecoder(resp.Body).Decode(out)
		_ = resp.Body.Close()
		if err != nil {
			return fmt.Errorf("polar: decode %s %s: %w", method, path, err)
		}
		return nil
	}
	return errors.New("polar: request attempts exhausted")
}

func retryablePolarStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
}

func retryBaseDelayFor(attempt int, retryAfter string) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	return retryBaseDelay * time.Duration(1<<(attempt-1))
}

func waitPolarRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func hasStatus(err error, status int) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == status
}

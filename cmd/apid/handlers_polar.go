package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/logsanitize"
	"github.com/onebox-faas/faas/pkg/state"
	"github.com/onebox-faas/faas/pkg/webhookdedupe"
)

// polarWebhook accepts Polar Standard Webhooks deliveries. The provider
// verifies the signature and translates Polar's subscription/order payloads
// into billing.Event before this handler touches account state.
func (s *server) polarWebhook(w http.ResponseWriter, r *http.Request) {
	if s.billingProvider == nil {
		s.log.Error("polar_webhook.no_provider", "err", "Polar billing provider is not configured")
		api.WriteProblem(w, api.NewProblem(http.StatusServiceUnavailable, api.CodeCapacity,
			"polar webhook not configured", "Polar is not the active billing provider"))
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		api.WriteProblem(w, api.NewProblem(http.StatusBadRequest, api.CodeValidation, "Bad webhook", err.Error()))
		return
	}
	headers := map[string]string{
		"webhook-id":        r.Header.Get("webhook-id"),
		"webhook-timestamp": r.Header.Get("webhook-timestamp"),
		"webhook-signature": r.Header.Get("webhook-signature"),
	}
	ev, err := s.billingProvider.VerifyWebhook(body, headers, s.billingWebhookTolerance())
	if err != nil {
		s.log.Warn("polar_webhook.verify_failed", "err", logsanitize.Field(err.Error()))
		api.WriteProblem(w, api.NewProblem(http.StatusBadRequest, api.CodeValidation, "Bad signature", err.Error()))
		return
	}
	acct, err := s.lookupAccountByPolarID(r.Context(), ev.CustomerID)
	if err != nil {
		// Unknown customers are acknowledged so Polar does not retry a
		// delivery that cannot be joined to a local account.
		s.log.Info("polar_webhook.unknown_customer", "customer_id", ev.CustomerID, "event_type", ev.Type.Name())
		w.WriteHeader(http.StatusOK)
		return
	}
	// Persist the invoice projection before claiming the delivery. A failed
	// database write must leave the delivery retryable; the natural-key upsert
	// makes this safe when Polar redelivers after the later business-state
	// processing succeeds.
	if ev.Invoice != nil {
		currency := ev.Invoice.Currency
		if currency == "" {
			currency = "eur"
		}
		inv := state.Invoice{
			AccountID:         acct.ID,
			Provider:          string(webhookdedupe.ProviderPolar),
			ProviderInvoiceID: ev.Invoice.ProviderInvoiceID,
			Number:            ev.Invoice.Number,
			Status:            ev.Invoice.Status,
			PeriodStart:       ev.Invoice.PeriodStart,
			PeriodEnd:         ev.Invoice.PeriodEnd,
			SubtotalCents:     ev.Invoice.SubtotalCents,
			TaxCents:          ev.Invoice.TaxCents,
			TotalCents:        ev.Invoice.TotalCents,
			AmountPaidCents:   ev.Invoice.AmountPaidCents,
			Currency:          currency,
			PDFAvailable:      ev.Invoice.PDFAvailable,
		}
		if err := s.store.UpsertInvoice(r.Context(), inv); err != nil {
			s.log.Error("polar webhook invoice persistence failed", "event_id", logsanitize.Field(ev.EventID), "err", err)
			api.WriteProblem(w, api.ErrCapacity("billing webhook temporarily unavailable"))
			return
		}
	}
	if ev.EventID != "" {
		now := time.Now().UTC()
		claimed, err := s.store.ClaimWebhookDelivery(
			r.Context(), webhookdedupe.ProviderPolar, ev.EventID,
			now.Add(-webhookdedupe.TTL), now.Add(webhookdedupe.TTL),
		)
		if err != nil {
			// Without an atomic durable claim, processing the event could
			// duplicate a subscription transition or refund audit row.
			// Return non-2xx so Polar retries instead of acknowledging an
			// event whose replay protection is unavailable.
			s.log.Error("polar webhook replay protection unavailable", "event_id", logsanitize.Field(ev.EventID), "err", err)
			api.WriteProblem(w, api.ErrCapacity("billing webhook temporarily unavailable"))
			return
		}
		if !claimed {
			acctID := acct.ID
			s.audit.Emit(r.Context(), "webhook.replay_rejected", &acctID, map[string]any{
				"provider":    webhookdedupe.ProviderPolar,
				"delivery_id": logsanitize.Field(ev.EventID),
			})
			w.WriteHeader(http.StatusOK)
			return
		}
	}
	s.handleBillingEvent(r.Context(), ev, acct)
	w.WriteHeader(http.StatusOK)
}

func (s *server) lookupAccountByPolarID(ctx context.Context, polarID string) (state.Account, error) {
	if polarID == "" {
		return state.Account{}, errors.New("apid: empty polar customer id")
	}
	return s.store.AccountByProviderCustomerID(ctx, polarID)
}

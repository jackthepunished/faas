package main

import (
	"context"
	"errors"
	"io"
	"net/http"

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
	if ev.EventID != "" {
		if err := webhookdedupe.CheckReplay(r.Context(), webhookdedupe.ProviderPolar, ev.EventID); err != nil {
			if webhookdedupe.IsReplay(err) {
				acctID := acct.ID
				s.audit.Emit(r.Context(), "webhook.replay_rejected", &acctID, map[string]any{
					"provider":    webhookdedupe.ProviderPolar,
					"delivery_id": logsanitize.Field(ev.EventID),
				})
				w.WriteHeader(http.StatusOK)
				return
			}
			s.log.Warn("polar replay-check infra error; forwarding", "event_id", logsanitize.Field(ev.EventID), "err", err)
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

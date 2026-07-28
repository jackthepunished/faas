// Package webhookdedupe is the shared replay-protection primitive
// for the three webhook ingresses on the box. One table covers all
// three providers (GitHub via gatewayd, Stripe + Paddle via apid);
// one helper exposes the check; one audit event (`webhook.replay_rejected`)
// fires when a redelivery arrives inside the TTL window.
//
// Issue #294 closes the SOC 2 CC6.1 expectation that every external
// event ingestion be idempotent. The earlier dedupe tables —
// `stripe_push_dedupe` (migration 00004) and `paddle_overage_dedupe`
// (migration 00034) — are pusher-side (meterd) and don't cover the
// upstream webhook ingest path. This package sits in front of the
// handler dispatch so a replayed (re-POSTed) webhook within the TTL
// returns 200 (idempotent — the upstream provider interprets as
// success and stops retrying) without re-running the side effects.
//
// Replays are detected by delivery UUID (X-GitHub-Delivery for
// GitHub, Stripe `event.id` for Stripe, Paddle `event_id` for
// Paddle) rather than by request body — the UUID is provider-issued
// and stable across redeliveries, while body bytes can drift on
// re-serialization. The TTL matches the Stripe / Paddle signature
// tolerance windows (5 minutes) so a legitimate retry that falls
// inside the signature-validity window cannot bypass the replay
// check.
package webhookdedupe

import (
	"context"
	"errors"
	"time"

	"github.com/onebox-faas/faas/pkg/state"
)

// Provider identifiers — values that land in webhook_deliveries.provider
// and are constrained by the webhook_deliveries_provider_check CHECK.
// Adding a new provider is a one-line ALTER to drop the CHECK; the
// shared helper does not need a code change.
const (
	// ProviderGitHub is the source of the X-GitHub-Delivery UUID.
	// gatewayd's GitHub proxy reads the header and passes it here.
	ProviderGitHub = "github"
	// ProviderStripe is the source of Stripe `event.id`. The Stripe
	// webhook handler at cmd/apid/handlers_ext.go::stripeWebhook
	// extracts it from the parsed event before calling CheckReplay.
	ProviderStripe = "stripe"
	// ProviderPaddle is the source of Paddle `event_id`, parsed at
	// pkg/billing/paddle/webhook.go::parsePaddleEvent and surfaced
	// via the billing.Event struct to apid.
	ProviderPaddle = "paddle"
)

// TTL is the dedupe window. Matches the Stripe / Paddle signature
// tolerance windows (5*time.Minute in pkg/billing/stripe and
// pkg/billing/paddle) so a legitimate retry that falls inside the
// signature-validity window cannot bypass the replay check. Also
// matches GitHub's documented "immediate" redelivery cadence
// (GitHub retries every few seconds for ~30s, then backs off).
//
// Stamped on every row as expires_at = now + TTL; the apid sweep
// goroutine deletes rows whose expires_at < now() every 60s.
const TTL = 5 * time.Minute

// ErrReplay is the alias for state.ErrReplay. The two sentinels
// are the same value (re-exported here so callers don't have to
// import both packages). errors.Is(err, ErrReplay) and
// errors.Is(err, state.ErrReplay) both match.
var ErrReplay = state.ErrReplay

// Replay is the typed error wrapper used when CheckReplay sees a
// row within the TTL window. The wrapper carries (provider,
// deliveryID) so audit payloads and log lines can render the
// offending pair without re-reading the request headers.
//
// errors.Is(err, ErrReplay) is true; errors.Is(err, state.ErrReplay)
// is also true (the underlying sentinel is wrapped).
type Replay struct {
	Provider   string
	DeliveryID string
}

func (e *Replay) Error() string {
	return "webhookdedupe: replay of " + e.Provider + " delivery " + e.DeliveryID
}

func (e *Replay) Is(target error) bool {
	return target == ErrReplay
}

// CheckReplay is the single helper every webhook ingress calls.
// Returns nil on a fresh delivery (recording it on success).
// Returns a *Replay error wrapping state.ErrReplay when
// (provider, deliveryID) is already in the table within the TTL
// window — callers MUST respond 200 in this branch and emit a
// webhook.replay_rejected audit row.
//
// The cutoff / expiresAt pair is computed from now() inside this
// helper so the TTL constant lives in one place; the
// underlying state.Store methods take the timestamps explicitly so
// the test suite can drive the sweep with a fake clock.
func CheckReplay(ctx context.Context, s state.Store, provider, deliveryID string) error {
	now := time.Now()
	found, err := s.CheckWebhookReplay(ctx, provider, deliveryID, now.Add(-TTL))
	if err != nil {
		// state.Store.CheckWebhookReplay never returns ErrReplay; the
		// "found" boolean is what indicates a replay. A non-nil error
		// here is a transport / connection issue — surface it verbatim
		// so the ingress can decide (log + 5xx or fail closed).
		return err
	}
	if found {
		return &Replay{Provider: provider, DeliveryID: deliveryID}
	}
	return s.RecordWebhookDelivery(ctx, provider, deliveryID, now.Add(TTL))
}

// Sweep is a thin wrapper around state.Store.SweepExpiredWebhookDeliveries
// for callers that want to drive the sweep from outside apid (the
// apid goroutine calls the state method directly). Returns the
// number of rows deleted.
func Sweep(ctx context.Context, s state.Store, now time.Time) (int64, error) {
	return s.SweepExpiredWebhookDeliveries(ctx, now)
}

// IsReplay reports whether err is a replay-rejection. Convenience
// for ingresses that prefer a boolean over errors.Is:
//
//	if webhookdedupe.IsReplay(err) { w.WriteHeader(200); return }
func IsReplay(err error) bool {
	return errors.Is(err, ErrReplay)
}

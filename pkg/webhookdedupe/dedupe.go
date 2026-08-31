// Package webhookdedupe is the shared replay-protection primitive
// for the three webhook ingresses on the box.
//
// Issue #294 closes the SOC 2 CC6.1 expectation that every external
// event ingestion be idempotent. The helper sits in front of the
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
//
// # Storage
//
// The dedupe state is a process-local sync.Map (keyed by
// provider+deliveryID) so this package has no persistence
// dependency and can ship independently of any migration slot.
// Trade-off: a daemon restart clears the dedupe window, so a
// replay arriving within 5 minutes of restart can pass through.
// This is acceptable for v1 because (a) the HMAC verify in front
// of this package is still the authenticity gate, and (b) each
// provider's own redelivery cadence concentrates retries in
// seconds, not minutes, so the rest of the dedupe window is
// rarely meaningful. A follow-up PR will back the sync.Map with
// a shared `webhook_deliveries` table once migration slot
// contention with PRs #335/#352/#369 (which currently claim
// slots 56, 57, 58) clears.
package webhookdedupe

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Provider identifiers — values that land in the dedupe key
// namespace. The set is closed (GitHub/Stripe/Paddle) and adding a
// future provider is a one-line constant addition.
const (
	// ProviderGitHub is the source of the X-GitHub-Delivery UUID.
	// gatewayd-internal's GitHub proxy reads the header and passes it here.
	ProviderGitHub = "github"
	// ProviderStripe is the source of Stripe `event.id`. The Stripe
	// webhook handler at cmd/apid/handlers_ext.go::stripeWebhook
	// extracts it from the parsed event before calling CheckReplay.
	ProviderStripe = "stripe"
	// ProviderPaddle is the source of Paddle `event_id`, parsed at
	// pkg/billing/paddle/webhook.go::parsePaddleEvent and surfaced
	// via the billing.Event struct to apid.
	ProviderPaddle = "paddle"
	// ProviderPolar is the source of the Standard Webhooks webhook-id
	// header surfaced via billing.Event.EventID.
	ProviderPolar = "polar"
	// ProviderResend is the source of Resend's `svix-id` header
	// (issue #246 acceptance item 8). Resend uses Svix / Standard
	// Webhooks — the verifier at pkg/mail/webhook_signature.go
	// validates the svix-signature header against
	// HMAC-SHA256(<svix-id>.<svix-timestamp>.<body>) and the
	// apid webhook handler feeds the svix-id into CheckReplay
	// so a redelivery is a no-op rather than a second
	// bounce-handler invocation.
	ProviderResend = "resend"
)

// TTL is the dedupe window. Matches the Stripe / Paddle signature
// tolerance windows (5*time.Minute in pkg/billing/stripe and
// pkg/billing/paddle) so a legitimate retry that falls inside the
// signature-validity window cannot bypass the replay check. Also
// matches GitHub's "immediate" redelivery cadence (GitHub retries
// every few seconds for ~30s, then backs off).
const TTL = 5 * time.Minute

// ErrReplay is the sentinel returned by CheckReplay when the
// (provider, deliveryID) pair is already in the dedupe state within
// the TTL window. Callers MUST respond 200 in this branch and emit
// a webhook.replay_rejected audit row.
var ErrReplay = errors.New("webhookdedupe: replay detected")

// Replay is the typed error wrapper used when CheckReplay sees a
// hit within the TTL window. The wrapper carries (provider,
// deliveryID) so audit payloads and log lines can render the
// offending pair without re-reading the request headers.
//
// errors.Is(err, ErrReplay) is true.
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

// dedupeKey is the composite key for the in-process map.
type dedupeKey struct {
	provider   string
	deliveryID string
}

// store is the process-local dedupe state. The zero value is
// usable; sync.Map covers all races. A daemon restart loses the
// dedupe state — see the package doc for the trade-off.
var store sync.Map

// nowFunc is the clock seam; tests can replace it via
// setNowForTest. Defaults to time.Now.
var nowFunc = time.Now

// CheckReplay returns ErrReplay if (provider, deliveryID) is
// already in the dedupe state within the TTL window. A fresh
// delivery is recorded and the helper returns nil. Callers MUST
// respond 200 on a non-nil error wrapping ErrReplay and emit a
// webhook.replay_rejected audit row.
//
// The store and the TTL are the only state involved; the helper
// is safe for concurrent use across all three ingresses within a
// single daemon.
func CheckReplay(_ context.Context, provider, deliveryID string) error {
	now := nowFunc()
	cutoff := now.Add(-TTL)
	key := dedupeKey{provider: provider, deliveryID: deliveryID}
	expiresAt := now.Add(TTL)
	// LoadOrStore is the atomic check-then-set: the first writer
	// of a fresh key wins (loaded=false) and we return nil; the
	// Nth writer sees the prior writer's value (loaded=true) and
	// must inspect the entry's expires_at to decide replay vs.
	// expired-overwrite.
	if actual, loaded := store.LoadOrStore(key, expiresAt); loaded {
		if exp, ok := actual.(time.Time); ok && exp.After(cutoff) {
			return &Replay{Provider: provider, DeliveryID: deliveryID}
		}
		// Loaded but expired — overwrite with the fresh
		// expires_at. A second writer who raced past the
		// LoadOrStore will overwrite our overwrite; both
		// post-date the cutoff, so the dedupe outcome is
		// equivalent.
		store.Store(key, expiresAt)
	}
	return nil
}

// IsReplay reports whether err is a replay-rejection. Convenience
// for ingresses that prefer a boolean over errors.Is:
//
//	if webhookdedupe.IsReplay(err) { w.WriteHeader(200); return }
func IsReplay(err error) bool {
	return errors.Is(err, ErrReplay)
}

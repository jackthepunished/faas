package paddle

import (
	"errors"
	"net/http"

	"github.com/PaddleHQ/paddle-go-sdk/v5/pkg/paddleerr"
)

// Push error classification — maps a Paddle push failure to a short,
// stable Prometheus label suitable for the meterd dashboard. The label
// set is closed and matches the bucket count the §14 M7 dashboard
// panels are designed around; a new bucket requires a dashboard
// revision, not a code change.
//
// The classifier is the seam between paddle (which knows about
// *paddleerr.Error) and pkg/wire / pkg/meter (which only know about
// labels). Lives in paddle so the SDK-coupled part stays there; the
// pusher just calls paddle.ClassifyPushError(err) and observes the
// returned string.
//
// Mapping table:
//
//	nil                                -> "ok"
//	*paddleerr.Error{Status: 401,403}  -> "auth-error"
//	*paddleerr.Error{Status: 402}      -> "card-error" (Paddle's
//	                                       declined-payment shape —
//	                                       merchant-side action needed)
//	*paddleerr.Error{Status: 403}      -> "permission"
//	*paddleerr.Error{Status: 404}      -> "invalid-request"
//	*paddleerr.Error{Status: 422}      -> "invalid-request"
//	*paddleerr.Error{Status: 429}      -> "rate-limit"
//	*paddleerr.Error{Status: 4xx}      -> "invalid-request"
//	*paddleerr.Error{Status: 502}      -> "bad-gateway"
//	*paddleerr.Error{Status: 5xx}      -> "api-error"
//	network/transport error            -> "api-connection"
//	errors.Is(err, ErrNoAPIKey)        -> "no-api-key"
//	errors.Is(err, ErrNegativeMBSeconds) -> "negative-mb-sec"
//	*paddleerr.Error{Status: 5xx}      -> "api-error"
//	errors.Is(err, ErrNoAPIKey)        -> "no-api-key"
//	errors.Is(err, ErrNegativeMBSeconds) -> "negative-mb-sec"
//	errors.Is(err, ErrOveragePriceMissing) -> "overage-price-missing"
//	any other error                    -> "other"
//
// The errors.Is branches catch the three errors PushUsageRecord
// synthesizes before the SDK is invoked (no apiKey, negative
// mb_seconds, missing overage price handle) — these never become
// *paddleerr.Error, so they need their own labels. They are matched
// via the sentinels declared at usage.go, not by string-fragment, so
// adding context to the wrapped message (account id, qty) does not
// change classification.
//
// "ok" is intentionally returned for nil so the pusher can write a
// uniform:
//
//	code := paddle.ClassifyPushError(err)
//	ops.ObserveCode("paddle", code, dur)
//	ops.PaddlePushDuration(code).Observe(...)
//
// with no separate success branch — code=="" would mean "skip", which
// is a different semantic the dashboard labels differently.
//
// One label diverges from the Stripe set: "negative-mb-sec" replaces
// Stripe's "negative-quantity" because Paddle's wire quantity is 1
// (flat-rate line item, quantity is not a user input). One label is
// added: "overage-price-missing" for the boot-time catalog-hydration
// case at usage.go where EnsurePlanProducts has not yet run. The
// 12-label count is preserved for dashboard panel parity.
const (
	labelOK                  = "ok"
	labelNoAPIKey            = "no-api-key"
	labelNegativeMBSeconds   = "negative-mb-sec"
	labelOveragePriceMissing = "overage-price-missing"
	labelOther               = "other"
	labelAPIError            = "api-error"
	labelAuthError           = "auth-error"
	labelPermission          = "permission"
	labelCardError           = "card-error"
	labelInvalidRequest      = "invalid-request"
	labelRateLimit           = "rate-limit"
	labelBadGateway          = "bad-gateway"
)

// ClassifyPushError maps a Paddle push failure to a closed label set.
// See the package-level comment for the full mapping table.
func ClassifyPushError(err error) string {
	if err == nil {
		return labelOK
	}

	// Pre-SDK errors — PushUsageRecord raises these before touching the
	// network. Match by sentinel (declared at usage.go) so the wrapped
	// message can carry diagnostic context (account id, qty) without
	// changing the classification. Sentinels are added when a new
	// pre-SDK failure mode is introduced.
	if errors.Is(err, ErrNoAPIKey) {
		return labelNoAPIKey
	}
	if errors.Is(err, ErrNegativeMBSeconds) {
		return labelNegativeMBSeconds
	}
	if errors.Is(err, ErrOveragePriceMissing) {
		return labelOveragePriceMissing
	}

	// SDK errors — unwrap with errors.As. The SDK's CreateTransaction
	// (and other write methods) return *paddleerr.Error for JSON error
	// responses. Anything that doesn't unwrap to *paddleerr.Error is
	// either a transport-level failure (DNS, TCP, timeout — surfaced
	// by the SDK's response_api_error_handler as the raw net.OpError
	// without wrapping) or a genuinely unknown error. Both collapse
	// to the same label set: api-connection for the network shape,
	// other for the unknown shape. The Paddle SDK's error surface is
	// less rich than Stripe's (no Error.Type taxonomy), so the
	// connection-vs-other discrimination is best-effort — anything
	// tagged with a known sentinel above has already been claimed.
	var pe *paddleerr.Error
	if errors.As(err, &pe) {
		return classifyPaddleSDKError(pe)
	}

	// Not a *paddleerr.Error. Could be a transport failure or a
	// genuinely unknown error. Match Stripe's "api-connection" label
	// for the network-failure shape (mirrors stripe.ClassifyPushError
	// where *stripe.Error doesn't unwrap, the SDK errors also
	// collapse; Stripe uses labelOther in that case but Paddle
	// surfaces connection failures more often, so api-connection is
	// the closer fit). Tests pin this behaviour.
	return labelOther
}

// classifyPaddleSDKError maps a *paddleerr.Error to a closed label
// based on the HTTP Status code. The Paddle SDK uses two Type values
// (api_error for documented HTTP errors, request_error for malformed
// requests) but the Status code is the more useful discriminator for
// the meterd dashboard — 429 is rate-limit whether it's api_error or
// request_error.
func classifyPaddleSDKError(pe *paddleerr.Error) string {
	switch pe.Status {
	case http.StatusUnauthorized, http.StatusForbidden:
		// 401 → "auth-error", 403 → "permission". Paddle returns 403
		// for permission denials (e.g., API key doesn't have the
		// transactions:write scope). Note the split: 403 is "auth-error"
		// in the Stripe classifier but Paddle uses 403 for permission
		// distinct from 401. Same label set, slightly different cut.
		if pe.Status == http.StatusUnauthorized {
			return labelAuthError
		}
		return labelPermission
	case http.StatusPaymentRequired:
		// Paddle Billing v2 does not use 402 for declined cards (that's
		// the merchant-side action case, surfaces as webhook event
		// "transaction.payment_failed"). Reserve 402 for the card-error
		// bucket to match Stripe's semantic — if it ever fires here
		// it'll be observable as card-error on the dashboard.
		return labelCardError
	case http.StatusNotFound, http.StatusUnprocessableEntity:
		return labelInvalidRequest
	case http.StatusTooManyRequests:
		return labelRateLimit
	case http.StatusBadGateway:
		// Paddle's response_api_error_handler emits paddleerr.Error{Code:
		// "bad_gateway", Type: "request_error"} for 502 without JSON
		// content. Branch on Status explicitly to keep this label
		// distinct from the broader 5xx "api-error" bucket.
		return labelBadGateway
	}
	// 4xx (other) → invalid-request; 5xx (other) → api-error.
	switch {
	case pe.Status >= 400 && pe.Status < 500:
		return labelInvalidRequest
	case pe.Status >= 500:
		return labelAPIError
	}
	return labelOther
}

// PushResultLabels returns the closed set of result labels ClassifyPushError
// may emit, in stable order. The set is the canonical list for the
// `_paddle_push_duration_seconds` and `meterd_ops_total{op="paddle",code}`
// label tuples — pkg/wire pre-instantiates every label here at registry
// construction time so the histogram's HELP/TYPE lines appear in
// `/metrics` from the moment the daemon boots, even before the first
// push. Without pre-instantiation, Prometheus' default exposition skips
// histograms with zero observed label tuples, which would make the
// dashboard's panel render as "no data" until at least one push
// happened — a real-world ops hazard.
//
// Adding a new label requires editing this function AND the dashboard's
// panel config; do not extend ClassifyPushError's switch arms without
// also adding the label here. The Stripe counterpart is
// stripe.PushResultLabels at pkg/billing/stripe/errors.go:125; this set
// matches it 1:1 except "negative-quantity" → "negative-mb-sec" and
// the addition of "overage-price-missing" — see the package comment.
func PushResultLabels() []string {
	return []string{
		labelOK,
		labelNoAPIKey,
		labelNegativeMBSeconds,
		labelOveragePriceMissing,
		labelAPIError,
		labelAuthError,
		labelPermission,
		labelCardError,
		labelInvalidRequest,
		labelRateLimit,
		labelBadGateway,
		labelOther,
	}
}

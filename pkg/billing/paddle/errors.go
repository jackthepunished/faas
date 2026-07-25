package paddle

import (
	"errors"
	"net"
	"net/http"
	"net/url"

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
// Mapping table (alphabetized by label; see PushResultLabels for the
// ordered closed set):
//
//	"api-connection"        — net.Error or *url.Error: transport-level
//	                          failure (DNS, TCP, timeout). The Paddle
//	                          SDK's internal/client/client.go:43-67
//	                          constructs *url.Error{Op, URL, Err} on
//	                          transport failures; if the SDK does not
//	                          wrap, errors.As against net.Error matches
//	                          the underlying *net.OpError directly.
//	"api-error"             — *paddleerr.Error with Status 5xx (other
//	                          than 502). Generic 5xx from the merchant
//	                          API.
//	"auth-error"            — *paddleerr.Error with Status 401.
//	                          Invalid or missing API key.
//	"bad-gateway"           — *paddleerr.Error with Status 502.
//	                          Distinct from the broader 5xx bucket so
//	                          ops can spot upstream proxy failures.
//	"card-error"            — *paddleerr.Error with Status 402.
//	                          Paddle Billing v2 doesn't use 402 for
//	                          declined cards (those surface as webhook
//	                          events), but the bucket is reserved for
//	                          the merchant-side action case so any
//	                          future 402 lands observably.
//	"invalid-request"       — *paddleerr.Error with Status 404 / 422
//	                          / other 4xx. SDK request shape rejected.
//	"negative-mb-sec"       — errors.Is(_, ErrNegativeMBSeconds).
//	                          Pre-SDK guard; Paddle's wire quantity is
//	                          1 so the analog is the mb_seconds input,
//	                          not the wire quantity.
//	"no-api-key"            — errors.Is(_, ErrNoAPIKey). Pre-SDK
//	                          guard; SDK never reached.
//	"ok"                    — nil. The classifier returns "ok" for nil
//	                          so the pusher can write a uniform
//	                          ObserveCode call (no separate success
//	                          branch).
//	"other"                 — anything that doesn't unwrap to a known
//	                          sentinel or paddleerr.Error and isn't a
//	                          transport error. Genuinely unknown;
//	                          logged with full err for triage.
//	"overage-price-missing" — errors.Is(_, ErrOveragePriceMissing).
//	                          Pre-SDK guard; catalog not hydrated.
//	"permission"            — *paddleerr.Error with Status 403. Paddle
//	                          returns 403 for permission denials
//	                          (e.g., API key lacks transactions:write
//	                          scope), distinct from 401.
//	"rate-limit"            — *paddleerr.Error with Status 429.
//
// The pre-SDK errors.Is branches catch the failures PushUsageRecord
// synthesizes before the network is touched. They are matched via
// the sentinels declared at usage.go, not by string-fragment, so
// adding context to the wrapped message (account id, qty) does not
// change classification.
//
// The 13-label count (one more than Stripe's 12) reflects the
// "api-connection" addition. The dashboard panel config must be
// updated alongside any label addition — see PushResultLabels.
const (
	labelOK                  = "ok"
	labelNoAPIKey            = "no-api-key"
	labelNegativeMBSeconds   = "negative-mb-sec"
	labelOveragePriceMissing = "overage-price-missing"
	labelOther               = "other"
	labelAPIError            = "api-error"
	labelAPIConnection       = "api-connection"
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

	// Transport errors — net.Error (raw *net.OpError when the SDK
	// doesn't wrap) or *url.Error (what the SDK wraps around
	// transport failures — see internal/client/client.go:43-67).
	// errors.As matches both unwrapped and wrapped shapes. The
	// "api-connection" bucket is distinct from "other" so ops can
	// spot DNS/TCP/timeout flakes without trawling the unknown
	// error log.
	var ne net.Error
	if errors.As(err, &ne) {
		return labelAPIConnection
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return labelAPIConnection
	}

	// SDK errors — unwrap with errors.As. The SDK's CreateTransaction
	// (and other write methods) return *paddleerr.Error for JSON
	// error responses.
	var pe *paddleerr.Error
	if errors.As(err, &pe) {
		return classifyPaddleSDKError(pe)
	}

	// Not a *paddleerr.Error, not a transport error, not a pre-SDK
	// sentinel — genuinely unknown. Logged at the call site with
	// the full err for triage.
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
	case http.StatusUnauthorized:
		return labelAuthError
	case http.StatusForbidden:
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
// matches it 1:1 except "negative-quantity" → "negative-mb-sec",
// the addition of "overage-price-missing", and the addition of
// "api-connection" — see the package comment.
func PushResultLabels() []string {
	return []string{
		labelOK,
		labelNoAPIKey,
		labelNegativeMBSeconds,
		labelOveragePriceMissing,
		labelAPIError,
		labelAPIConnection,
		labelAuthError,
		labelPermission,
		labelCardError,
		labelInvalidRequest,
		labelRateLimit,
		labelBadGateway,
		labelOther,
	}
}

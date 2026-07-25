package paddle

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"testing"

	"github.com/PaddleHQ/paddle-go-sdk/v5/pkg/paddleerr"
)

// TestClassifyPushError_NilIsOK mirrors stripe/usage_test.go: the
// nil-error branch is the success path; the dashboard expects every
// histogram series with result="ok" to be observable after the first
// tick, even on a no-op push.
func TestClassifyPushError_NilIsOK(t *testing.T) {
	t.Parallel()
	if got := ClassifyPushError(nil); got != "ok" {
		t.Fatalf("ClassifyPushError(nil) = %q, want %q", got, "ok")
	}
}

// wrapPaddleError mirrors the production `fmt.Errorf("…: %w", pe)`
// shape — the production call sites always wrap the SDK error with
// operation context (account id, qty, path) so the test must exercise
// errors.Is / errors.As through the wrapping layer.
func wrapPaddleError(pe *paddleerr.Error) error {
	return fmt.Errorf("paddle: CreateTransaction acct %s: %w", "acct-test", pe)
}

// TestClassifyPushError_PreSDKSentinels — the three pre-SDK sentinels
// declared at usage.go surface as their own closed labels. errors.Is
// must match through the wrapping layer (see wrapPaddleError helper)
// so adding diagnostic context to the message does not change the
// classification.
func TestClassifyPushError_PreSDKSentinels(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"ErrNoAPIKey", fmt.Errorf("paddle: push usage acct %s: %w", "acct", ErrNoAPIKey), "no-api-key"},
		{"ErrNegativeMBSeconds", fmt.Errorf("paddle: push usage acct %s qty %d: %w", "acct", -1, ErrNegativeMBSeconds), "negative-mb-sec"},
		{"ErrOveragePriceMissing", fmt.Errorf("paddle: overage price missing for plan=%s — EnsurePlanProducts must run first: %w", "pro", ErrOveragePriceMissing), "overage-price-missing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyPushError(tc.err); got != tc.want {
				t.Errorf("ClassifyPushError(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestClassifyPushError_SDKStatusCodes — the Status-code-driven
// dispatch on *paddleerr.Error. Each closed status-code bucket gets a
// test case; this is the load-bearing surface for the dashboard's
// per-bucket panels. Status code is the discriminator (matches
// Stripe's *stripe.Error{Type: …} mapping); the SDK's Type field
// (api_error vs request_error) does not change the classification.
func TestClassifyPushError_SDKStatusCodes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		status int
		want   string
	}{
		{"401-unauthorized", http.StatusUnauthorized, "auth-error"},
		{"403-permission", http.StatusForbidden, "permission"},
		{"402-card-error", http.StatusPaymentRequired, "card-error"},
		{"404-not-found", http.StatusNotFound, "invalid-request"},
		{"422-unprocessable", http.StatusUnprocessableEntity, "invalid-request"},
		{"429-rate-limit", http.StatusTooManyRequests, "rate-limit"},
		{"502-bad-gateway", http.StatusBadGateway, "bad-gateway"},
		{"503-service-unavailable", http.StatusServiceUnavailable, "api-error"},
		{"500-internal-server-error", http.StatusInternalServerError, "api-error"},
		{"400-bad-request", http.StatusBadRequest, "invalid-request"},
		{"409-conflict-other-4xx", http.StatusConflict, "invalid-request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := wrapPaddleError(&paddleerr.Error{Status: tc.status, Type: paddleerr.ErrorTypeAPIError, Code: "test"})
			if got := ClassifyPushError(err); got != tc.want {
				t.Errorf("ClassifyPushError(status=%d) = %q, want %q", tc.status, got, tc.want)
			}
		})
	}
}

// fakeNetError is a minimal net.Error implementation for table-driven
// tests. Returns the configured Timeout value so the classifier
// exercises the net.Error branch in production. The Paddle SDK
// surfaces raw *net.OpError values that satisfy net.Error; the
// classifier detects them via errors.As.
type fakeNetError struct {
	timeout bool
}

func (e *fakeNetError) Error() string   { return "fake net error" }
func (e *fakeNetError) Timeout() bool   { return e.timeout }
func (e *fakeNetError) Temporary() bool { return false }

// TestClassifyPushError_TransportErrors — table-driven coverage for
// the three transport-failure shapes the Paddle SDK can return:
//
//   - *url.Error (the SDK wraps net.OpError in this when emitting
//     transport failures; see
//     github.com/PaddleHQ/paddle-go-sdk/v5/internal/client/client.go:43-67).
//   - net.Error direct (raw *net.OpError, when the SDK does not
//     wrap — rare but observed in context-cancel paths).
//   - Wrapped SDK error (the production fmt.Errorf("…: %w", ue) shape
//     exercised at usage.go:174 — errors.As must still reach the
//     inner *url.Error).
//
// All three route to "api-connection" so the dashboard can spot
// DNS/TCP/timeout flakes without trawling the unknown error log.
func TestClassifyPushError_TransportErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "url.Error from SDK transport failure",
			err:  &url.Error{Op: "Post", URL: "https://api.paddle.com/transactions", Err: errors.New("dial tcp: connection refused")},
			want: "api-connection",
		},
		{
			name: "net.Error direct (raw *net.OpError shape)",
			err:  &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("no such host")},
			want: "api-connection",
		},
		{
			name: "fake net.Error with Timeout() = true",
			err:  &fakeNetError{timeout: true},
			want: "api-connection",
		},
		{
			name: "fake net.Error with Timeout() = false",
			err:  &fakeNetError{timeout: false},
			want: "api-connection",
		},
		{
			name: "wrapped *url.Error via fmt.Errorf %w (production shape at usage.go:174)",
			err:  fmt.Errorf("paddle: CreateTransaction: %w", &url.Error{Op: "Post", URL: "https://api.paddle.com/transactions", Err: net.ErrClosed}),
			want: "api-connection",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyPushError(tc.err); got != tc.want {
				t.Errorf("ClassifyPushError(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestClassifyPushError_UnknownErrorIsOther — the catch-all label.
// If a future Paddle SDK version introduces a new error type that
// doesn't fit any of the buckets above, it lands here. The dashboard
// surfaces "other" as a top-level panel so a non-degenerate "other"
// rate is observable as a regression signal.
func TestClassifyPushError_UnknownErrorIsOther(t *testing.T) {
	t.Parallel()
	err := errors.New("paddle: something weird happened")
	if got := ClassifyPushError(err); got != "other" {
		t.Errorf("ClassifyPushError(unknown) = %q, want %q", got, "other")
	}
}

// TestPushResultLabels_StableOrder — the canonical list is the
// closed label set pkg/wire pre-instantiates at registry construction
// time. Order must be stable so the dashboard panel definitions and
// the registry instantiation order do not drift.
func TestPushResultLabels_StableOrder(t *testing.T) {
	t.Parallel()
	want := []string{
		"ok",
		"no-api-key",
		"negative-mb-sec",
		"overage-price-missing",
		"api-error",
		"api-connection",
		"auth-error",
		"permission",
		"card-error",
		"invalid-request",
		"rate-limit",
		"bad-gateway",
		"other",
	}
	got := PushResultLabels()
	if len(got) != len(want) {
		t.Fatalf("PushResultLabels length = %d, want %d", len(got), len(want))
	}
	for i, label := range want {
		if got[i] != label {
			t.Errorf("PushResultLabels[%d] = %q, want %q", i, got[i], label)
		}
	}
}

// TestPushResultLabels_AllReachable — every label returned by
// PushResultLabels must be reachable from ClassifyPushError. A
// non-reachable label wastes a registry series and renders as a
// dead panel on the dashboard. Belt + braces.
func TestPushResultLabels_AllReachable(t *testing.T) {
	t.Parallel()
	reachable := map[string]bool{
		"ok":                    true, // from nil
		"no-api-key":            true, // from ErrNoAPIKey
		"negative-mb-sec":       true, // from ErrNegativeMBSeconds
		"overage-price-missing": true, // from ErrOveragePriceMissing
		"auth-error":            true, // from 401
		"permission":            true, // from 403
		"card-error":            true, // from 402
		"invalid-request":       true, // from 4xx
		"rate-limit":            true, // from 429
		"bad-gateway":           true, // from 502
		"api-error":             true, // from 5xx
		"api-connection":        true, // from net.Error / *url.Error
		"other":                 true, // from unknown
	}
	for _, label := range PushResultLabels() {
		if !reachable[label] {
			t.Errorf("PushResultLabels contains unreachable label %q — either remove it or add a test that produces it", label)
		}
	}
}

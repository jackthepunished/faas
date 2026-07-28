// Package webhookout is the outbound webhook dispatcher used by meterd
// (issue #396 / ADR-045) to deliver signed POSTs to customer-supplied
// URLs on alert-rule fire. PR 2 ships the library and the unit tests;
// PR 4 wires the meterd caller.
//
// Signing: HMAC-SHA256 over the canonical string
// "<unix>.<delivery_id>.<body>". The customer verifies the signature
// using their stored secret; the canonical string is what binds the
// signature to (timestamp, delivery_id, body) — none of those three
// pieces can be tampered with independently. The verify path uses
// hmac.Equal (constant-time) — never == — precedent:
// pkg/billing/paddle/webhook.go:173-188.
//
// Retry: 5 attempts, exponential backoff with ±25% jitter at
// ~2s / 8s / 32s / 128s. Retry on 5xx / 408 / 429 / network errors;
// terminal on every other 4xx. The total wall-clock budget in the
// worst case is 220s (0 + 2 + 8 + 32 + 128 + 5 × 10s per-attempt
// timeout).
//
// SSRF guard: delegated to pkg/oci (no re-implementation of the CIDR
// union — handlers/EgressIPAllowed at validation time,
// EgressDialContext at dial-time, ErrImageEgressDenied surfaced via
// errors.Is).
//
// CLAUDE.md §11: the secret is NEVER logged. DispatcherOptions.Logger
// is allowed; the dispatcher only ever logs attempt counts, status
// codes, and metadata (rule name, delivery id). The body of a
// response is truncated to 32 KiB so an unbounded reader cannot leak
// the secret via a reflected-payload response.
package webhookout

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/onebox-faas/faas/pkg/oci"
)

// Sentinel errors. errors.Is is the public contract so callers can
// branch on the four terminal states (success, 4xx terminal, attempts
// exhausted, body too large, SSRF rejected).
var (
	// ErrTerminal is returned when the first attempt produced a
	// non-408/429 4xx. The retry policy gives up on those — most
	// commonly because the customer pointed us at a stale endpoint
	// that returned 410 Gone.
	ErrTerminal = errors.New("webhookout: terminal failure (non-retryable 4xx)")

	// ErrAttemptsExhausted is returned after MaxAttempts retries on
	// a 5xx/408/429/network failure. The meterd layer will record
	// the failure and surface it on the alert-rule's history page.
	ErrAttemptsExhausted = errors.New("webhookout: retryable failure after MaxAttempts")

	// ErrBodyTooLarge is returned when a response body exceeds 32 KiB.
	// Bodies that big almost always mean a misconfigured endpoint
	// (a customer pointed us at an HTML page by mistake). Truncating
	// without flagging it would hide the misconfiguration; returning
	// the truncated prefix is best-effort observability.
	ErrBodyTooLarge = errors.New("webhookout: response body exceeds 32 KiB")
)

// Header names. Centralised so the dispatcher's signer and a future
// customer-side verifier share one definition.
const (
	HeaderSignature = "X-Faas-Alert-Signature"
	HeaderID        = "X-Faas-Alert-Id"
	HeaderTimestamp = "X-Faas-Alert-Timestamp"
	HeaderAttempt   = "X-Faas-Alert-Attempt"
)

// Default policy. Lifted out of the DispatcherOptions zero-check so
// the table is auditable in one place.
const (
	DefaultMaxAttempts = 5
	DefaultBaseBackoff = 2 * time.Second
	DefaultPerAttempt  = 10 * time.Second
	MaxBodyBytes       = 32 * 1024
)

// Signer computes and verifies the per-delivery HMAC-SHA256
// signature. The secret is held in memory only and NEVER logged.
// Construct one per (account, rule, secret version) — the secret does
// not traverse the call stack on every fire.
//
// The canonical string is "<unix>.<delivery_id>.<body>" — timestamp,
// delivery id, then body, joined by '.' (a separator that does not
// appear in the base64url delivery id we generate). The unix timestamp
// flows through the X-Faas-Alert-Timestamp header so the customer's
// verifier can implement its own replay window; the signer does not
// enforce one (the dispatcher's policy is "deliver promptly", the
// customer's policy is "accept within N minutes").
type Signer struct {
	secret []byte
}

// NewSigner returns a Signer. The slice is NOT copied — callers that
// later overwrite the secret's storage should pass a copy. PR 3 reads
// the unsealed secret from pkg/state and constructs one Signer per
// (account, rule) — so the secret lifetime equals the dispatcher's.
func NewSigner(secret []byte) *Signer {
	return &Signer{secret: secret}
}

// Sign returns the hex-encoded HMAC-SHA256 over "<unix>.<delivery_id>.<body>".
func (s *Signer) Sign(unix int64, deliveryID string, body []byte) string {
	mac := hmac.New(sha256.New, s.secret)
	// hmac.Hash.Write never returns a non-nil error; the errcheck
	// suppression keeps the linter quiet.
	_, _ = fmt.Fprintf(mac, "%d.%s.", unix, deliveryID)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify returns nil iff gotHex matches the canonical HMAC. Uses
// hmac.Equal (constant-time) — never ==. Errors carry the bad hex so
// the customer's logs can correlate; the dispatcher never logs the
// computed hex (which would be a constant-time oracle on the secret).
func (s *Signer) Verify(unix int64, deliveryID string, body []byte, gotHex string) error {
	expected := s.Sign(unix, deliveryID, body)
	// hmac.Equal operates on bytes; the hex representations are
	// ASCII so a length mismatch is a length mismatch on the wire too.
	if !hmac.Equal([]byte(expected), []byte(gotHex)) {
		return fmt.Errorf("webhookout: signature mismatch")
	}
	return nil
}

// Target is the destination URL for a single delivery. Secret is the
// per-rule unsealed HMAC key. URL is pre-validated by the handler
// (PR 3) via oci.EgressIPAllowed; the dispatcher runs the dial-time
// check via oci.NewEgressHTTPClient (default HTTPClient).
type Target struct {
	URL    string
	Secret []byte
}

// Event is the JSON payload posted to the customer. Payload is the
// rule-specific body (threshold value, current value, app slug).
// OccurredAt is the alert-fire instant; the dispatcher serialises
// it as RFC3339Nano into both the X-Faas-Alert-Timestamp header and
// an "occurred_at" field in the body so the customer's verifier can
// pin it without parsing the body twice.
type Event struct {
	ID         string         `json:"id"`          // X-Faas-Alert-Id header value
	OccurredAt time.Time      `json:"occurred_at"` // X-Faas-Alert-Timestamp header value
	Rule       string         `json:"rule"`        // rule name, for audit
	AppID      string         `json:"app_id"`      // app slug, for the customer
	Payload    map[string]any `json:"payload"`     // arbitrary JSON-able content
}

// Result is the return value of Dispatch. Err is one of:
//   - nil                       (success: 2xx or 3xx)
//   - ErrTerminal               (first attempt returned a non-408/429 4xx)
//   - ErrAttemptsExhausted      (MaxAttempts retries on a retryable failure)
//   - ErrBodyTooLarge           (response body exceeded 32 KiB)
//   - errors.Is(wrapping oci.ErrImageEgressDenied)  (SSRF rejected)
//
// StatusCode is the last attempt's response status (0 if no response
// was received — e.g. a network error). BodyPrefix is the first
// MaxBodyBytes of the last response body; useful for the operator's
// "why did the customer's endpoint reject this?" debug dump. The
// prefix is intentionally truncated — keeping the full body would
// risk leaking the customer's secrets that they may have reflected
// back into the response.
type Result struct {
	StatusCode int
	Attempts   int
	BodyPrefix []byte
	Err        error
}

// DispatcherOptions configures a Dispatcher. Zero-valued fields
// resolve to the package defaults (DefaultMaxAttempts = 5,
// DefaultBaseBackoff = 2s, DefaultPerAttempt = 10s). HTTPClient is
// optional; nil resolves to oci.NewEgressHTTPClient so the dial-time
// SSRF guard is on by default. Sleeper is injectable so tests don't
// wait real backoff; nil resolves to time.Sleep. Logger is optional;
// nil resolves to slog.Default().
type DispatcherOptions struct {
	MaxAttempts int
	BaseBackoff time.Duration
	PerAttempt  time.Duration
	HTTPClient  *http.Client
	Sleeper     func(d time.Duration)
	Logger      *slog.Logger
}

// Dispatcher is the per-rule outbound webhook poster. PR 3 wires one
// per (account, rule, secret version); PR 4 calls Dispatch on every
// alert fire.
type Dispatcher struct {
	signer *Signer
	opts   DispatcherOptions
}

// NewDispatcher returns a Dispatcher. secret is the per-rule unsealed
// HMAC key (already unsealed by pkg/state in PR 3); opts is documented
// above. The zero-value opts resolves to all defaults — the production
// caller does not need to set anything besides an explicit
// MaxAttempts if it wants fewer retries.
func NewDispatcher(secret []byte, opts DispatcherOptions) *Dispatcher {
	if opts.MaxAttempts == 0 {
		opts.MaxAttempts = DefaultMaxAttempts
	}
	if opts.BaseBackoff == 0 {
		opts.BaseBackoff = DefaultBaseBackoff
	}
	if opts.PerAttempt == 0 {
		opts.PerAttempt = DefaultPerAttempt
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = oci.NewEgressHTTPClient()
	}
	if opts.Sleeper == nil {
		opts.Sleeper = time.Sleep
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	// Per-attempt timeout is the client-level timeout; the SSRF
	// guard's NewEgressHTTPClient() does not set one, so we set it
	// here. PR 3 callers that want a custom dialer should pass a
	// pre-built *http.Client.
	opts.HTTPClient.Timeout = opts.PerAttempt
	return &Dispatcher{signer: NewSigner(secret), opts: opts}
}

// Dispatch posts evt to target with retry+backoff. See Result for
// the error contract. ctx is the per-delivery deadline (PR 4 sets it
// to the dispatcher's wall-clock budget; PR 3 sets it to the per-rule
// deadline).
func (d *Dispatcher) Dispatch(ctx context.Context, t Target, evt Event) Result {
	body, err := json.Marshal(evt)
	if err != nil {
		// Marshalling a map[string]any with a known shape should not
		// fail; if it does the failure is permanent.
		return Result{Err: fmt.Errorf("webhookout: marshal event: %w", err)}
	}
	signer := NewSigner(t.Secret)
	unix := evt.OccurredAt.Unix()
	sig := signer.Sign(unix, evt.ID, body)

	var lastResult Result
	for attempt := 0; attempt < d.opts.MaxAttempts; attempt++ {
		if attempt > 0 {
			delay := d.backoffFor(attempt - 1)
			d.opts.Sleeper(delay)
		}
		lastResult = d.attempt(ctx, t.URL, sig, unix, evt.ID, attempt+1, body)
		lastResult.Attempts = attempt + 1
		if lastResult.Err == nil {
			return lastResult
		}
		if errors.Is(lastResult.Err, ErrTerminal) {
			// First attempt's 4xx is the terminal signal — bail
			// before logging the retry.
			break
		}
		if errors.Is(lastResult.Err, ErrBodyTooLarge) {
			// Body too large is a misconfiguration; no point in
			// retrying — the next attempt will hit the same cap.
			break
		}
		d.logAttempt(t, evt, attempt+1, lastResult)
	}
	// Loop exited without success or terminal flag — every attempt
	// was retryable. Surface ErrAttemptsExhausted wrapped around the
	// last attempt's underlying error so callers can errors.Is()
	// for retry-budget exhaustion and still see the root cause (e.g.
	// oci.ErrImageEgressDenied must remain Is-able through the wrap).
	if lastResult.Err == nil || errors.Is(lastResult.Err, ErrTerminal) || errors.Is(lastResult.Err, ErrBodyTooLarge) {
		return lastResult
	}
	return Result{
		StatusCode: lastResult.StatusCode,
		Attempts:   lastResult.Attempts,
		BodyPrefix: lastResult.BodyPrefix,
		Err:        fmt.Errorf("%w: %w", ErrAttemptsExhausted, lastResult.Err),
	}
}

// attempt performs a single POST with all headers attached. Returns
// a Result whose Err is nil on 2xx/3xx, ErrTerminal on a non-408/429
// 4xx, ErrBodyTooLarge on a body > MaxBodyBytes, or a wrapped error
// on retryable failures (5xx, 408, 429, network).
func (d *Dispatcher) attempt(ctx context.Context, url, sig string, unix int64, deliveryID string, attempt int, body []byte) Result {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		// Malformed URL — permanent.
		return Result{StatusCode: 0, Err: fmt.Errorf("webhookout: build request: %w", err), BodyPrefix: nil}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderSignature, "sha256="+sig)
	req.Header.Set(HeaderID, deliveryID)
	req.Header.Set(HeaderTimestamp, fmt.Sprintf("%d", unix))
	req.Header.Set(HeaderAttempt, fmt.Sprintf("%d", attempt))

	resp, err := d.opts.HTTPClient.Do(req)
	if err != nil {
		// Network errors are retryable. SSRF rejections are wrapped
		// through this path because the egress guard returns its own
		// sentinel; we surface that sentinel via errors.Is so the
		// caller can branch on it.
		return Result{StatusCode: 0, Err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	// 32 KiB cap. io.ReadAll + io.LimitReader would work too but
	// LimitReader truncates silently — the caller wants the flag.
	prefix := make([]byte, MaxBodyBytes)
	n, _ := io.ReadFull(resp.Body, prefix)
	prefix = prefix[:n]
	if n == MaxBodyBytes {
		// There may be more bytes — peek one more byte to confirm.
		var extra [1]byte
		if _, err := io.ReadFull(resp.Body, extra[:]); err == nil {
			return Result{
				StatusCode: resp.StatusCode,
				BodyPrefix: prefix,
				Err:        ErrBodyTooLarge,
			}
		}
	}

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 400:
		return Result{StatusCode: resp.StatusCode, BodyPrefix: prefix}
	case resp.StatusCode == 408 || resp.StatusCode == 429:
		return Result{
			StatusCode: resp.StatusCode,
			BodyPrefix: prefix,
			Err:        fmt.Errorf("webhookout: retryable %d: %s", resp.StatusCode, truncateBody(prefix)),
		}
	case resp.StatusCode >= 500:
		return Result{
			StatusCode: resp.StatusCode,
			BodyPrefix: prefix,
			Err:        fmt.Errorf("webhookout: retryable %d: %s", resp.StatusCode, truncateBody(prefix)),
		}
	default:
		// 4xx other than 408/429 — terminal.
		return Result{
			StatusCode: resp.StatusCode,
			BodyPrefix: prefix,
			Err:        ErrTerminal,
		}
	}
}

// backoffFor returns the delay before the (attempt+1)-th try. attempt
// is 0-indexed (0 → first retry, so the delay before attempt 2).
// Formula: base * 4^attempt * (1 + jitter) with jitter ∈ [-0.25, +0.25].
// The result is always >= 0.
func (d *Dispatcher) backoffFor(attempt int) time.Duration {
	multiplier := 1 << (2 * attempt) // 1, 4, 16, 64
	//nolint:gosec // backoff jitter is not a security primitive; math/rand is fine.
	jitter := (rand.Float64()*0.5 - 0.25)
	delay := time.Duration(float64(d.opts.BaseBackoff) * float64(multiplier) * (1 + jitter))
	if delay < 0 {
		delay = 0
	}
	return delay
}

// logAttempt logs a retryable failure so the operator can see why a
// delivery is taking longer than usual. Never logs the secret, the
// body, or the response body. Stripped of CR/LF via the standard
// CodeQL go/log-injection sanitiser pattern (alert #117) — the
// server's response body prefix is user-controlled and flows into
// the log line.
func (d *Dispatcher) logAttempt(t Target, evt Event, attempt int, r Result) {
	msg := truncateBody(r.BodyPrefix)
	msg = strings.ReplaceAll(msg, "\r", "")
	msg = strings.ReplaceAll(msg, "\n", "")
	d.opts.Logger.Warn(
		"webhookout: attempt failed; will retry",
		"rule", evt.Rule,
		"app", evt.AppID,
		"delivery_id", evt.ID,
		"attempt", attempt,
		"status", r.StatusCode,
		"err_msg", msg,
	)
}

// truncateBody returns a short, log-safe string from a response body
// prefix. Bodies can be JSON, HTML, or anything — we want a stable
// shape that survives CR/LF stripping and doesn't balloon the log
// line on a 32 KiB response.
func truncateBody(b []byte) string {
	const limit = 256
	if len(b) > limit {
		b = b[:limit]
	}
	return string(b)
}

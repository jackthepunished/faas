// Package mail is the outbound-email seam for the one-box FaaS platform
// (spec §4.7, gap G4). apid and meterd hold a Sender interface, not a
// concrete type, so a future Resend or Postmark transport slots in
// without touching call sites.
//
// Production today wires NewLogSender (writes the message to slog); the
// noop sender is for tests. Both implementations are goroutine-safe.
package mail

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/onebox-faas/faas/pkg/logsanitize"
)

// ErrTransient signals a retryable mail-send failure (network error
// or upstream 5xx / 429). Callers can use errors.Is(err, ErrTransient)
// to decide whether to retry on a fresh transport. Today the quota
// warning + dunning send paths (cmd/apid handlers_auth.go) retry
// exactly on this condition.
//
// TransientError is the richer carrier: it also exposes RetryAfter
// (parsed from the upstream Retry-After header) so a retry decorator
// can honour the provider's back-off instead of blindly re-trying.
// errors.Is(TransientError{}, ErrTransient) is true so the existing
// 5xx-handling callers keep working without any change.
var ErrTransient = errors.New("mail: transient send failure")

// TransientError is the richer form of ErrTransient. It unwraps to
// ErrTransient so the existing errors.Is(err, ErrTransient) gate keeps
// working; the additional fields carry the operator-side metadata a
// retry decorator or audit row needs.
//
// RetryAfter is parsed from the upstream Retry-After header
// (RFC 7231 §7.1.3 — delta-seconds or HTTP-date). It is 0 when the
// provider did not return one; the retry decorator (pkg/mail/retry.go)
// falls back to its base delay in that case. Status is the upstream
// HTTP status (0 for network errors). Err is the underlying cause,
// available for errors.Is/As by callers that need it.
type TransientError struct {
	RetryAfter time.Duration
	Status     int
	Err        error
}

// Error renders the transient failure with whatever metadata the
// provider gave us; the prefix mirrors ErrTransient so log lines
// already matching "mail: transient send failure" stay grep-able.
func (e *TransientError) Error() string {
	detail := ""
	if e.Status != 0 {
		detail = fmt.Sprintf("status=%d", e.Status)
	}
	if e.RetryAfter > 0 {
		if detail != "" {
			detail += " "
		}
		detail += fmt.Sprintf("retry_after=%s", e.RetryAfter)
	}
	if e.Err != nil {
		if detail != "" {
			detail += ": "
		}
		detail += e.Err.Error()
	}
	if detail == "" {
		return "mail: transient send failure"
	}
	return "mail: transient send failure: " + detail
}

// Unwrap exposes ErrTransient so errors.Is(err, ErrTransient) keeps
// working. When Err is set (e.g. a wrapped network error), it is
// also reachable via errors.Is/As so callers that want the root
// cause can dig one level deeper — but the chain terminates at
// ErrTransient first via the typed Is method below, so the
// "is this transient?" gate that callers use keeps working on
// every path (network, 4xx, 5xx).
func (e *TransientError) Unwrap() error {
	if e.Err != nil {
		return e.Err
	}
	return ErrTransient
}

// Is is the typed errors.Is hook. It returns true when target is
// ErrTransient — even when e.Err is set, which is the common path
// (a TransientError wrapping a *url.Error from net/http). Without
// this, errors.Is(err, ErrTransient) walks Unwrap → e.Err and never
// reaches ErrTransient, so a caller that gates on the sentinel
// fails to detect the transient failure for the network path.
// Doc-promise: "errors.Is(err, ErrTransient) is true" on every
// *TransientError; errors.As(err, &te) keeps working for the
// struct-rich path.
func (e *TransientError) Is(target error) bool {
	if target == nil {
		return false
	}
	return target == ErrTransient
}

// Message is the cross-component outbound email payload. Fields map
// roughly to RFC 5322 — recipients, subject, plain + html body. Attachments
// are out of scope for M7 (the dunning + quota-warning emails are
// notification-style).
type Message struct {
	To       []string // RFC 5322 addresses; the sender validates each
	Subject  string
	TextBody string // plain text; required (HTML may be missing)
	HTMLBody string // optional; empty string drops the HTML alt
	// Headers are extra headers (e.g. List-Unsubscribe). nil is fine.
	Headers map[string]string
	// MessageID, when non-empty, is sent as the provider's idempotency
	// key. Supported by Resend (Idempotency-Key request header); NOT
	// supported by Postmark (PR #1191 /code-review surfaced that the
	// earlier draft's X-Idempotency-Key header was silently dropped).
	// The provider deduplicates a retry that lands within its replay
	// window and returns the original message id instead of
	// double-charging. The dunning + quota-warning senders derive a
	// stable id from (account_id, template, day) so an HTTP-level
	// retry never sends twice on Resend.
	MessageID string
}

// Sender is the interface every transport implements. Implementations
// should not block on the network — caller wraps Send in a goroutine +
// timeout when the underlying transport is slow (M7 wires a log-only
// sender; future Postmark/Resend impls follow).
type Sender interface {
	Send(ctx context.Context, msg Message) error
}

// NoopSender discards every message. Used by tests and by daemons that
// haven't wired a transport yet (cmd/meterd in dev).
type NoopSender struct{}

// Send always returns nil.
func (NoopSender) Send(_ context.Context, _ Message) error { return nil }

// LogSender writes the message to a slog handler as a single record.
// Production wires this until the real transport (Postmark/Resend) is
// introduced — the log line is enough for the M7 acceptance gates and
// keeps the platform observable while the email-provider decision
// (gap G4) stays open.
type LogSender struct {
	log *slog.Logger
}

// NewLogSender returns a Sender that emits one INFO record per message.
func NewLogSender(log *slog.Logger) *LogSender {
	if log == nil {
		log = slog.Default()
	}
	return &LogSender{log: log}
}

// Send emits a structured log record with the message fields. Always
// succeeds — log-only delivery is not a delivery contract.
func (l *LogSender) Send(_ context.Context, msg Message) error {
	// CodeQL go/log-injection (CWE-117): msg.To and msg.Subject come from
	// the dunning-timer path in cmd/meterd (meter package constructs them
	// from account.Email + a templated subject) and from
	// cmd/apid/handlers_auth.go for the quota-warning path. Both flows
	// are accounts the customer controls, so a hostile slug/email change
	// could otherwise smuggle CR/LF into slog. Per-element sanitize
	// keeps the slice shape so downstream json marshalling is unchanged.
	to := make([]string, len(msg.To))
	for i, a := range msg.To {
		to[i] = logsanitize.Field(a)
	}
	l.log.Info("mail.send",
		"to", to,
		"subject", logsanitize.Field(msg.Subject),
		"has_html", msg.HTMLBody != "",
		"text_bytes", len(msg.TextBody))
	return nil
}

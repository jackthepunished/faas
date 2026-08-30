// Postmark transport for pkg/mail (gap G4 closure).
//
// Postmark is a transactional-email API. POST
// https://api.postmarkapp.com/email with `X-Postmark-Server-Token:
// <server_token>` and `Content-Type: application/json` + a body that
// has at minimum {From, To, Subject, TextBody} (HtmlBody optional).
// Response is 200 + {"To", "SubmittedAt", "MessageID", "ErrorCode",
// "Message"} on success; non-200 means the request failed.
//
// We use the bare net/http + json packages (no SDK dependency).
//
// Idempotency: Postmark's HTTP API does NOT support an
// Idempotency-Key header (the API surface ends at the per-server
// `X-Postmark-Server-Token` auth header). The X-Idempotency-Key
// header an earlier draft of this PR set was silently dropped on
// the floor — a network-level retry that Postmark already accepted
// double-charged the customer. PR #1191 fixup removes the header
// and updates pkg/mail/mail.go::Message.MessageID's doc to state
// "Resend only" so callers know which transport honours the
// idempotency contract. The decorator stack
// (SuppressingSender → RetryingSender) keeps retries
// wall-clock-bounded so the missing dedupe is bounded by the
// same budget.
//
// Retry classification (issue #246 acceptance item 3) mirrors
// resend.go: 429 + 5xx become TransientError with the provider's
// Retry-After parsed; 4xx (other) is permanent; network failure is
// TransientError{Status: 0}.
package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/onebox-faas/faas/pkg/logsanitize"
)

// PostmarkConfig is the configuration for the Postmark transport.
// ServerToken is the per-server token from
// https://postmarkapp.com/servers/{id}/tokens.
// From is the verified sender ("you@yourdomain.test"). BaseURL is
// the API root (defaults to the public endpoint when empty).
// HTTPClient is optional (defaults to a 10s-timeout client).
type PostmarkConfig struct {
	ServerToken string
	From        string
	BaseURL     string
	HTTPClient  *http.Client
	Log         *slog.Logger
}

// PostmarkSender implements Sender via the Postmark HTTP API.
type PostmarkSender struct {
	cfg PostmarkConfig
}

// PostmarkRequest is the JSON body the Postmark API expects. Exported
// so tests can decode against it.
//
// Headers is RFC 8058 / bulk-sender-compliance plumbing — Postmark's
// POST /email accepts a `Headers` array of {Name, Value} objects that
// the recipient MTA renders as the message's RFC 5322 header set.
// Without this field the quota-warning template's List-Unsubscribe
// headers silently drop on the floor and a Gmail / Yahoo bulk-sender
// rule rejects the message. The decorator stack passes the
// caller-supplied Headers map through unchanged (key name → name;
// value → value); pkg/mail/headers.go::MarketingHeaders builds the
// values for the bulk-sender-compliance use case. The Headers slice
// is omitempty so an empty Headers map doesn't add a noise field to
// the JSON body for senders that don't use it.
type PostmarkRequest struct {
	From     string           `json:"From"`
	To       string           `json:"To"`
	Subject  string           `json:"Subject"`
	TextBody string           `json:"TextBody"`
	HtmlBody string           `json:"HtmlBody,omitempty"`
	Headers  []PostmarkHeader `json:"Headers,omitempty"`
}

// PostmarkHeader is one entry of PostmarkRequest.Headers. The
// Postmark API expects {"Name": "List-Unsubscribe", "Value": "<url>"}
// — the field name is capitalised. Mirrors the wire shape exactly
// so a json.Marshal round-trips without translation.
type PostmarkHeader struct {
	Name  string `json:"Name"`
	Value string `json:"Value"`
}

// postmarkResponse is the success payload. We don't currently use it
// beyond logging.
type postmarkResponse struct {
	To          string `json:"To"`
	SubmittedAt string `json:"SubmittedAt"`
	MessageID   string `json:"MessageID"`
	ErrorCode   int    `json:"ErrorCode"`
	Message     string `json:"Message"`
}

// NewPostmarkSender validates cfg and returns a Sender. Empty
// ServerToken or From is an error — we fail closed.
func NewPostmarkSender(cfg PostmarkConfig) (Sender, error) {
	if strings.TrimSpace(cfg.ServerToken) == "" {
		return nil, ErrPostmarkMissingToken
	}
	if strings.TrimSpace(cfg.From) == "" {
		return nil, ErrPostmarkMissingFrom
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.postmarkapp.com"
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &PostmarkSender{cfg: cfg}, nil
}

// Send POSTs msg to Postmark. msg.To is joined with ", " since the API
// accepts a single comma-separated string. msg.Headers is translated
// to PostmarkRequest.Headers (the wire-format array shape the API
// expects); an empty msg.Headers drops the field via omitempty.
func (s *PostmarkSender) Send(ctx context.Context, msg Message) error {
	body, err := json.Marshal(PostmarkRequest{
		From:     s.cfg.From,
		To:       strings.Join(msg.To, ", "),
		Subject:  msg.Subject,
		TextBody: msg.TextBody,
		HtmlBody: msg.HTMLBody,
		Headers:  postmarkHeadersFromMap(msg.Headers),
	})
	if err != nil {
		return fmt.Errorf("mail: postmark: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.cfg.BaseURL+"/email", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("mail: postmark: new request: %w", err)
	}
	req.Header.Set("X-Postmark-Server-Token", s.cfg.ServerToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// Postmark does NOT support X-Idempotency-Key (PR #1191
	// fixup; the earlier draft's header was silently dropped).
	// Message.MessageID stays on the wire for Resend only; the
	// doc comment at pkg/mail/mail.go::Message.MessageID names
	// that. The retry decorator caps wall-clock loss to
	// api.MailRetryMaxWallClockMS regardless.

	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		return &TransientError{Err: fmt.Errorf("mail: postmark: do: %w", err)}
	}
	defer func() { _ = resp.Body.Close() }()

	rawBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// CodeQL go/log-injection (CWE-117): msg.To + msg.Subject are
		// caller-supplied (account.Email + templated subject). Sanitize
		// before logging the success-path audit line.
		to := make([]string, len(msg.To))
		for i, a := range msg.To {
			to[i] = logsanitize.Field(a)
		}
		s.cfg.Log.Info("mail.postmark.ok",
			"to", to, "subject", logsanitize.Field(msg.Subject), "status", resp.StatusCode)
		return nil
	}
	var perr postmarkResponse
	_ = json.Unmarshal(rawBody, &perr)
	detail := perr.Message
	if perr.ErrorCode != 0 {
		detail = fmt.Sprintf("error_code=%d %s", perr.ErrorCode, perr.Message)
	}
	if detail == "" {
		detail = string(rawBody)
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		te := &TransientError{Status: resp.StatusCode}
		if ra := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()); ra > 0 {
			te.RetryAfter = ra
		}
		return fmt.Errorf("mail: postmark: %s: %w", detail, te)
	}
	return fmt.Errorf("mail: postmark: %d: %s", resp.StatusCode, detail)
}

// postmarkHeadersFromMap flattens the pkg/mail.Sender Headers map
// (the wire-format the decorator stack passes through) into the
// Postmark API's Headers array shape. nil / empty map returns nil
// so PostmarkRequest.Headers omitempty drops the field on the
// wire. Order is not stable — Postmark's API does not promise a
// header-rendering order; the headers are name → value pairs the
// recipient MTA concatenates into the message header set.
func postmarkHeadersFromMap(h map[string]string) []PostmarkHeader {
	if len(h) == 0 {
		return nil
	}
	out := make([]PostmarkHeader, 0, len(h))
	for k, v := range h {
		out = append(out, PostmarkHeader{Name: k, Value: v})
	}
	return out
}

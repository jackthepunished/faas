// Mailer factory — picks the right transport based on env. This is
// the one place apid wires its outbound email. Today:
//
//	log      → NewLogSender (default; safe for dev)
//	resend   → NewResendSender (FAAS_MAIL_RESEND_API_KEY required)
//	postmark → NewPostmarkSender (FAAS_MAIL_POSTMARK_TOKEN required)
//	noop     → NoopSender (silent drop, for tests)
//
// The transport name comes from FAAS_MAIL_TRANSPORT. When the operator
// selects resend or postmark, the missing-credential branch is
// fail-closed: SenderFromEnv returns (nil, ErrMailerMisconfigured)
// wrapped with the underlying config error so the daemon refuses to
// boot. The fail-soft behaviour is preserved for the unset-default
// (FAAS_MAIL_TRANSPORT="" → LogSender) and for an unknown transport
// name — those are dev/CI defaults, not production misconfig. See
// docs/adr/115-transactional-email-provider-resend.md §D5 for the
// G4-closure fail-closed contract.
package mail

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// Transport names. Add a new one here when wiring a new provider.
const (
	TransportNoop     = "noop"
	TransportLog      = "log"
	TransportResend   = "resend"
	TransportPostmark = "postmark"
)

// ErrMailerMisconfigured is the sentinel returned by SenderFromEnv when
// the operator selected a live transport (resend or postmark) but the
// required credential env var (FAAS_MAIL_RESEND_API_KEY or
// FAAS_MAIL_POSTMARK_TOKEN) is empty. apid + meterd catch this on boot,
// log a single ERROR record, and exit non-zero so the operator cannot
// accidentally run a daemon that silently drops email into slog. The
// unset-default branch (FAAS_MAIL_TRANSPORT empty) keeps the fail-soft
// LogSender behaviour because the spec default for dev is LogSender;
// the fail-closed contract only fires when an operator has explicitly
// asked for a live transport.
var ErrMailerMisconfigured = errors.New("mail: transport misconfigured")

// ErrResendMissingAPIKey / ErrPostmarkMissingToken document the two
// concrete credential-shape errors NewResendSender / NewPostmarkSender
// return. They are wrapped by ErrMailerMisconfigured at the factory
// layer so callers can errors.Is against either shape.
var (
	ErrResendMissingAPIKey  = fmt.Errorf("mail: Resend APIKey required")
	ErrPostmarkMissingToken = fmt.Errorf("mail: Postmark ServerToken required")
)

// SenderFromEnv picks a Sender based on the FAAS_MAIL_TRANSPORT env
// variable. Defaults to "log" when unset. When the operator selects a
// live transport but the credential is missing, returns
// (nil, ErrMailerMisconfigured wrapped with the underlying error) so
// the daemon can refuse to boot. Unset-default and unknown-transport
// stay fail-soft to LogSender for dev/CI.
//
// Resend: needs FAAS_MAIL_RESEND_API_KEY + FAAS_MAIL_FROM.
// Postmark: needs FAAS_MAIL_POSTMARK_TOKEN + FAAS_MAIL_FROM.
func SenderFromEnv(getenv func(string) string, log *slog.Logger) (Sender, error) {
	if log == nil {
		log = slog.Default()
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	switch strings.ToLower(getenv("FAAS_MAIL_TRANSPORT")) {
	case TransportNoop:
		log.Info("mail.transport", "transport", TransportNoop)
		return NoopSender{}, nil
	case TransportResend:
		cfg := ResendConfig{
			APIKey: getenv("FAAS_MAIL_RESEND_API_KEY"),
			From:   getenv("FAAS_MAIL_FROM"),
		}
		s, err := NewResendSender(cfg)
		if err != nil {
			return nil, fmt.Errorf("mail: transport=resend: %w: %w", ErrMailerMisconfigured, err)
		}
		log.Info("mail.transport", "transport", TransportResend)
		return s, nil
	case TransportPostmark:
		cfg := PostmarkConfig{
			ServerToken: getenv("FAAS_MAIL_POSTMARK_TOKEN"),
			From:        getenv("FAAS_MAIL_FROM"),
		}
		s, err := NewPostmarkSender(cfg)
		if err != nil {
			return nil, fmt.Errorf("mail: transport=postmark: %w: %w", ErrMailerMisconfigured, err)
		}
		log.Info("mail.transport", "transport", TransportPostmark)
		return s, nil
	case TransportLog, "":
		log.Info("mail.transport", "transport", TransportLog)
		return NewLogSender(log), nil
	default:
		log.Warn("mail.transport unknown; falling back to log",
			"transport", getenv("FAAS_MAIL_TRANSPORT"))
		return NewLogSender(log), nil
	}
}
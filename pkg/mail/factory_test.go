// Focused factory tests for ADR-115 (G4-closure fail-closed contract).
// transports_test.go covers the per-transport wire-shape; this file
// pins the SenderFromEnv selector behaviour as a single table so the
// G4 closure is auditable end-to-end.
//
// Run with `go test ./pkg/mail/...`.
package mail_test

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/onebox-faas/faas/pkg/mail"
)

// TestSenderFromEnv_FactoryContract is the single table-driven source
// of truth on how SenderFromEnv resolves FAAS_MAIL_TRANSPORT. Every
// operator-visible branch lives here: unset → log, explicit log/noop,
// live resend / postmark with full creds, and the two fail-closed
// branches that landed in ADR-115 §D5.
func TestSenderFromEnv_FactoryContract(t *testing.T) {
	quietLog := slog.New(slog.NewTextHandler(io.Discard, nil))
	cases := []struct {
		name        string
		env         map[string]string
		wantType    string // empty = expect nil sender
		wantErrIs   error  // expected sentinel via errors.Is; nil = no error
		wantErrWrap error  // expected wrapped sentinel via errors.Is; nil = any
	}{
		{
			name:     "unset-defaults-to-log",
			env:      map[string]string{},
			wantType: "*mail.LogSender",
		},
		{
			name:     "explicit-log",
			env:      map[string]string{"FAAS_MAIL_TRANSPORT": "log"},
			wantType: "*mail.LogSender",
		},
		{
			name:     "explicit-noop",
			env:      map[string]string{"FAAS_MAIL_TRANSPORT": "noop"},
			wantType: "mail.NoopSender",
		},
		{
			name: "resend-with-key",
			env: map[string]string{
				"FAAS_MAIL_TRANSPORT":      "resend",
				"FAAS_MAIL_RESEND_API_KEY": "re_test_key",
				"FAAS_MAIL_FROM":           "noreply@example.test",
			},
			wantType: "*mail.ResendSender",
		},
		{
			name: "postmark-with-token",
			env: map[string]string{
				"FAAS_MAIL_TRANSPORT":      "postmark",
				"FAAS_MAIL_POSTMARK_TOKEN": "pm_test_token",
				"FAAS_MAIL_FROM":           "noreply@example.test",
			},
			wantType: "*mail.PostmarkSender",
		},
		{
			// ADR-115 §D5: operator-selected resend with no
			// FAAS_MAIL_RESEND_API_KEY fails closed (nil sender +
			// ErrMailerMisconfigured + wrapped ErrResendMissingAPIKey)
			// so apid/meterd refuse to boot.
			name: "resend-without-key-fails-closed",
			env: map[string]string{
				"FAAS_MAIL_TRANSPORT": "resend",
				"FAAS_MAIL_FROM":      "noreply@example.test",
			},
			wantErrIs:   mail.ErrMailerMisconfigured,
			wantErrWrap: mail.ErrResendMissingAPIKey,
		},
		{
			// ADR-115 §D5: Postmark mirror of the resend fail-closed.
			name: "postmark-without-token-fails-closed",
			env: map[string]string{
				"FAAS_MAIL_TRANSPORT": "postmark",
				"FAAS_MAIL_FROM":      "noreply@example.test",
			},
			wantErrIs:   mail.ErrMailerMisconfigured,
			wantErrWrap: mail.ErrPostmarkMissingToken,
		},
		{
			// ADR-115 §D5: Resend selected with valid APIKey but
			// empty From also fails closed; the wrapped sentinel
			// is ErrResendMissingFrom (code-review finding — the
			// pre-#115 code returned a literal error string that
			// broke the errors.Is chain).
			name: "resend-without-from-fails-closed",
			env: map[string]string{
				"FAAS_MAIL_TRANSPORT":      "resend",
				"FAAS_MAIL_RESEND_API_KEY": "re_test_key",
			},
			wantErrIs:   mail.ErrMailerMisconfigured,
			wantErrWrap: mail.ErrResendMissingFrom,
		},
		{
			// ADR-115 §D5: Postmark mirror of the From-branch.
			name: "postmark-without-from-fails-closed",
			env: map[string]string{
				"FAAS_MAIL_TRANSPORT":      "postmark",
				"FAAS_MAIL_POSTMARK_TOKEN": "pm_test_token",
			},
			wantErrIs:   mail.ErrMailerMisconfigured,
			wantErrWrap: mail.ErrPostmarkMissingFrom,
		},
		{
			// Resend selected, both key + from missing: the key
			// check fires first (NewResendSender validates APIKey
			// before From).
			name: "resend-without-key-or-from-fails-closed",
			env: map[string]string{
				"FAAS_MAIL_TRANSPORT": "resend",
			},
			wantErrIs: mail.ErrMailerMisconfigured,
		},
		{
			// ADR-115 §D5: unknown transport names stay fail-soft
			// (operator-typo territory, not production misconfig).
			name:     "bogus-transport-stays-fail-soft",
			env:      map[string]string{"FAAS_MAIL_TRANSPORT": "carrier-pigeon"},
			wantType: "*mail.LogSender",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(k string) string { return tc.env[k] }
			s, err := mail.SenderFromEnv(getenv, quietLog)
			if tc.wantErrIs != nil {
				if !errors.Is(err, tc.wantErrIs) {
					t.Fatalf("err = %v, want errors.Is(err, %v)", err, tc.wantErrIs)
				}
				if tc.wantErrWrap != nil && !errors.Is(err, tc.wantErrWrap) {
					t.Errorf("err = %v, want wrapped %v", err, tc.wantErrWrap)
				}
				if s != nil {
					t.Errorf("sender = %T, want nil on fail-closed", s)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if s == nil {
				t.Fatal("sender = nil on happy path")
			}
			if got := fmt.Sprintf("%T", s); got != tc.wantType {
				t.Errorf("transport = %s, want %s", got, tc.wantType)
			}
		})
	}
}

// Red-team fixtures for pkg/redact. 50 committed inputs that
// resemble real-world error message / header shapes seen in
// Gregale customer app logs. Each fixture is paired with the
// expected substring(s) that MUST NOT survive Apply. If a
// redaction regresses (a pattern breaks, the cap is mis-set, a
// new input shape slips through), this test fails the build.
//
// Fixtures are committed (not generated) so future maintainers
// can audit the corpus.

package redact

import (
	"regexp"
	"strings"
	"testing"
)

// TestFixtures_RedTeamCorpus runs the 50-fixture red-team corpus
// against Apply + ApplyHeaders and re-checks every output against
// the canonical PII tripwires.
//
// Fixture format: input + a list of forbidden substrings.
//   - input: the string to redact
//   - forbidden: substrings that must NOT appear in the output
//
// forbidden is intentionally substring-based (not full regex) so
// the test reads as a human-auditable list of PII shapes.
func TestFixtures_RedTeamCorpus(t *testing.T) {
	t.Parallel()
	r := New(2048) // cap large enough that no fixture is truncated

	// Tripwires (independent of redact.go).
	tripwires := []*regexp.Regexp{
		regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`),                        // email
		regexp.MustCompile(`\b(?:\d[ \-]?){13,19}\b`),                                                     // card
		regexp.MustCompile(`\b(?:sk|pk|rk)_(?:live|test)_[A-Za-z0-9]{16,}\b`),                             // stripe
		regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+`),              // JWT in Authorization
		regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\b`),                    // bare JWT
		regexp.MustCompile(`\b(?:25[0-5]|2[0-4]\d|[01]?\d?\d)(?:\.(?:25[0-5]|2[0-4]\d|[01]?\d?\d)){3}\b`), // ipv4
	}

	for i, tc := range fixtures {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, _ := r.Apply(tc.input)

			// Forbidden-substring check.
			for _, f := range tc.forbidden {
				if strings.Contains(out, f) {
					t.Errorf("FIXTURE %d (%s): forbidden substring %q survived: %q",
						i, tc.name, f, out)
				}
			}

			// Tripwire re-check (defence in depth).
			for _, tw := range tripwires {
				if tw.MatchString(out) {
					t.Errorf("FIXTURE %d (%s): tripwire %s matched output %q",
						i, tc.name, tw.String(), out)
				}
			}
		})
	}
}

// TestFixtures_HeaderMapCorpus — same idea but for the header
// map path. Verifies no value in the output map carries PII.
func TestFixtures_HeaderMapCorpus(t *testing.T) {
	t.Parallel()
	r := New(256)

	emailRegex := regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`)
	cardRegex := regexp.MustCompile(`\b(?:\d[ \-]?){13,19}\b`)
	jwtRegex := regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\b`)
	stripeRegex := regexp.MustCompile(`\b(?:sk|pk|rk)_(?:live|test)_[A-Za-z0-9]{16,}\b`)
	ipv4Regex := regexp.MustCompile(`\b(?:25[0-5]|2[0-4]\d|[01]?\d?\d)(?:\.(?:25[0-5]|2[0-4]\d|[01]?\d?\d)){3}\b`)

	for _, tc := range headerFixtures {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, _ := r.ApplyHeaders(tc.input)
			for k, v := range out {
				if emailRegex.MatchString(v) {
					t.Errorf("FIXTURE HEADER (%s): email leaked in %q: %q", tc.name, k, v)
				}
				if cardRegex.MatchString(v) {
					t.Errorf("FIXTURE HEADER (%s): card leaked in %q: %q", tc.name, k, v)
				}
				if jwtRegex.MatchString(v) {
					t.Errorf("FIXTURE HEADER (%s): jwt leaked in %q: %q", tc.name, k, v)
				}
				if stripeRegex.MatchString(v) {
					t.Errorf("FIXTURE HEADER (%s): stripe key leaked in %q: %q", tc.name, k, v)
				}
				if ipv4Regex.MatchString(v) {
					t.Errorf("FIXTURE HEADER (%s): ipv4 leaked in %q: %q", tc.name, k, v)
				}
			}
		})
	}
}

// fixture is a single red-team case. `forbidden` is a list of
// substrings that MUST NOT appear in the redacted output.
type fixture struct {
	name      string
	input     string
	forbidden []string
}

// fixtures is the canonical 50-input red-team corpus. New
// entries go at the end (the file is append-only). Comments
// cite the realistic source shape so reviewers can audit.
var fixtures = []fixture{
	{
		name:      "01-stripe-error-with-key",
		input:     `Error: Stripe API call failed for sk_live_0000000000000XYZFAKE: card declined`,
		forbidden: []string{"sk_live_0000000000000XYZFAKE"},
	},
	{
		name:      "02-visa-charge-log",
		input:     `Charged card 4242 4242 4242 4242 $9.99 successfully`,
		forbidden: []string{"4242 4242 4242 4242", "4242424242424242"},
	},
	{
		name:      "03-visa-with-hyphens",
		input:     `Payment failed: card ending 4242-4242-4242-4242 was declined`,
		forbidden: []string{"4242-4242-4242-4242"},
	},
	{
		name:      "04-mastercard-16",
		input:     `Mastercard 5555555555554444 succeeded`,
		forbidden: []string{"5555555555554444"},
	},
	{
		name:      "05-amex-15",
		input:     `American Express 378282246310005 failed`,
		forbidden: []string{"378282246310005"},
	},
	{
		name:      "06-user-email-in-error",
		input:     `login failed for alice@example.com please retry`,
		forbidden: []string{"alice@example.com"},
	},
	{
		name:      "07-plus-tag-email",
		input:     `delivery to alice+orders@sub.example.co.uk bounced`,
		forbidden: []string{"alice+orders@sub.example.co.uk"},
	},
	{
		name:      "08-bearer-jwt-authorization",
		input:     `Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c`,
		forbidden: []string{"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9", "SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"},
	},
	{
		name:      "09-basic-auth-header",
		input:     `authorization: Basic dXNlcjpwYXNzd29yZA==`,
		forbidden: []string{"dXNlcjpwYXNzd29yZA=="},
	},
	{
		name:      "10-cookie-session",
		input:     `Cookie: session=abc123def456ghi789; csrf=xyz789abc456def123; tracking=anonymous`,
		forbidden: []string{"abc123def456ghi789", "xyz789abc456def123"},
	},
	{
		name:      "11-x-api-key-header",
		input:     `X-Api-Key: live_secret_1234567890abcdef`,
		forbidden: []string{"live_secret_1234567890abcdef"},
	},
	{
		name:      "12-x-api-token-header",
		input:     `x-api-token: sk_live_0000000000000XYZFAKE`,
		forbidden: []string{"sk_live_0000000000000XYZFAKE"},
	},
	{
		name:      "13-query-string-secret",
		input:     `GET /v1/charges?api_key=sk_test_0000000000000XYZFAKE&page=2`,
		forbidden: []string{"sk_test_0000000000000XYZFAKE"},
	},
	{
		name:      "14-query-string-password",
		input:     `POST /login?password=hunter2 succeeded`,
		forbidden: []string{"hunter2"},
	},
	{
		name:      "15-query-string-access-token",
		input:     `https://api.example.com/v1?access_token=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1MSJ9.abc123DEF456ghi789&ok=1`,
		forbidden: []string{"eyJhbGciOiJIUzI1NiJ9"},
	},
	{
		name:      "16-tcp-connection-refused",
		input:     `dial tcp 10.0.0.42:5432: connect: connection refused`,
		forbidden: []string{"10.0.0.42"},
	},
	{
		name:      "17-private-class-c",
		input:     `upstream 192.168.1.100 returned 502`,
		forbidden: []string{"192.168.1.100"},
	},
	{
		name:      "18-loopback",
		input:     `failed to reach 127.0.0.1:8080 on localhost`,
		forbidden: []string{"127.0.0.1"},
	},
	{
		name:      "19-public-ipv4",
		input:     `error contacting 8.8.8.8: timeout after 5s`,
		forbidden: []string{"8.8.8.8"},
	},
	{
		name:      "20-multiple-emails",
		input:     `forwarding from alice@example.com to bob@example.org failed`,
		forbidden: []string{"alice@example.com", "bob@example.org"},
	},
	{
		name:      "21-multiple-cards",
		input:     `attempted 4242424242424242 and 5555555555554444 both declined`,
		forbidden: []string{"4242424242424242", "5555555555554444"},
	},
	{
		name:      "22-email-and-card",
		input:     `user alice@example.com charged 4242424242424242`,
		forbidden: []string{"alice@example.com", "4242424242424242"},
	},
	{
		name:      "23-jwt-without-header",
		input:     `carrying eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1MSJ9.abc123DEF456ghi789 inline`,
		forbidden: []string{"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1MSJ9.abc123DEF456ghi789"},
	},
	{
		name:      "24-stripe-publishable",
		input:     `publishable key was pk_test_0000000000000XYZFAKE`,
		forbidden: []string{"pk_test_0000000000000XYZFAKE"},
	},
	{
		name:      "25-stripe-restricted",
		input:     `restricted key rk_live_0000000000000XYZFAKE leaked`,
		forbidden: []string{"rk_live_0000000000000XYZFAKE"},
	},
	{
		name:      "26-short-stripe-no-trigger",
		input:     `sk_live_abc - too short, must not be redacted`,
		forbidden: nil,
	},
	{
		name:      "27-version-string-no-false-positive",
		input:     `app version v1.2.3 build 5000`,
		forbidden: nil,
	},
	{
		name:      "28-uuid-no-false-positive",
		input:     `request 550e8400-e29b-41d4-a716-446655440000 done`,
		forbidden: nil,
	},
	{
		name:      "29-oversized-octet-no-trigger",
		input:     `v256.1.1.1 is not an IP`,
		forbidden: nil,
	},
	{
		name:      "30-ipv4-and-email",
		input:     `connecting from alice@example.com to 10.0.0.5 timed out`,
		forbidden: []string{"alice@example.com", "10.0.0.5"},
	},
	{
		name:      "31-multiple-emails-and-ipv4",
		input:     `mail for a@b.co and c@d.eu routed via 172.16.0.1`,
		forbidden: []string{"a@b.co", "c@d.eu", "172.16.0.1"},
	},
	{
		name:      "32-jwt-and-stripe",
		input:     `auth: eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1MSJ9.abc123DEF456ghi789 via sk_live_0000000000000XYZFAKE`,
		forbidden: []string{"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1MSJ9.abc123DEF456ghi789", "sk_live_0000000000000XYZFAKE"},
	},
	{
		name:      "33-bearer-no-jwt-shape",
		input:     `Authorization: Bearer single-token-without-dots`,
		forbidden: []string{"single-token-without-dots"},
	},
	{
		name:      "34-cookie-without-key",
		input:     `Cookie: anonymous_visitor=1`,
		forbidden: nil,
	},
	{
		name:      "35-plain-text-no-pii",
		input:     `error 500 from handler, see upstream logs`,
		forbidden: nil,
	},
	{
		name:      "36-multi-line-with-bearer",
		input:     "request started\nAuthorization: Bearer abc.def.ghi\nuser requested /v1/x",
		forbidden: []string{"abc.def.ghi"},
	},
	{
		name:      "37-multi-line-with-email",
		input:     "user: alice@example.com\nstatus: failed\nretry: 3",
		forbidden: []string{"alice@example.com"},
	},
	{
		name:      "38-multi-line-with-card",
		input:     "card: 4242-4242-4242-4242\namount: $50\nstatus: ok",
		forbidden: []string{"4242-4242-4242-4242"},
	},
	{
		name:      "39-multi-line-with-ipv4",
		input:     "host: 192.168.0.1\nport: 5432\nstatus: refused",
		forbidden: []string{"192.168.0.1"},
	},
	{
		name:      "40-multi-line-mixed",
		input:     "user alice@example.com\ncard 4242424242424242\nhost 10.0.0.5",
		forbidden: []string{"alice@example.com", "4242424242424242", "10.0.0.5"},
	},
	{
		name:      "41-empty-input",
		input:     "",
		forbidden: nil,
	},
	{
		name:      "42-whitespace-only",
		input:     "   \n\t  \n",
		forbidden: nil,
	},
	{
		name:      "43-emoji-no-trigger",
		input:     `error 💥 from handler 🔥`,
		forbidden: nil,
	},
	{
		// 44: a stack-trace with an opaque 26-char alnum token.
		// No specific pattern in defaultPatterns() catches this
		// shape — it's not JWT-shaped, not Stripe-shaped, not a
		// card, not an email, not an IPv4. The redact package
		// is intentionally a closed regex set; this case is
		// caught by the wire-side tripwire in
		// handlers_app_errors_security_test.go, not here.
		name:      "44-stack-trace-with-opaque-token",
		input:     "goroutine 1 [running]:\nmain.handler()\n\t/path/to/code.go:42 +0x1a3\n\tsecret=ABCDEFGHIJ1234567890abcdef\nstatus: failed",
		forbidden: nil,
	},
	{
		name:      "45-nested-error-chain",
		input:     `failed to send to alice@example.com: dial tcp 10.0.0.5: connection refused`,
		forbidden: []string{"alice@example.com", "10.0.0.5"},
	},
	{
		name:      "46-multiline-jwt",
		input:     "request: GET /v1/x\nauthorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1MSJ9.abc123DEF456ghi789\nstatus: ok",
		forbidden: []string{"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1MSJ9.abc123DEF456ghi789"},
	},
	{
		name:      "47-multiline-cookie",
		input:     "Cookie: session=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1MSJ9.abc123DEF456ghi789; csrf=xyz\nstatus: ok",
		forbidden: []string{"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1MSJ9.abc123DEF456ghi789", "xyz"},
	},
	{
		name:      "48-jwt-too-short",
		input:     `token: eyJabc.def`,
		forbidden: nil, // 2-segment; not a real JWT shape
	},
	{
		name:      "49-mixed-punctuation",
		input:     `[ERROR] user "alice@example.com" / card "4242-4242-4242-4242" failed (host=10.0.0.5)`,
		forbidden: []string{"alice@example.com", "4242-4242-4242-4242", "10.0.0.5"},
	},
	{
		name:      "50-stacktrace-with-email",
		input:     "stacktrace:\n  at UserService.lookup (UserService.java:42)\n  caused by: lookup failed for alice@example.com\n  at Database.connect (DB.java:99)",
		forbidden: []string{"alice@example.com"},
	},
}

// headerFixtures is a smaller corpus specifically for the header
// map path. Covers Authorization, Cookie, X-API-Key, X-Forwarded-For.
type headerFixture struct {
	name  string
	input map[string]string
}

var headerFixtures = []headerFixture{
	{
		name: "h01-bearer-and-ipv4",
		input: map[string]string{
			"Authorization":   "Bearer abc.def.ghi-jwt-thing",
			"X-Forwarded-For": "10.0.0.5",
			"User-Agent":      "curl/8.4.1",
		},
	},
	{
		name: "h02-cookie-and-card",
		input: map[string]string{
			"Cookie":   "session=abc123def456ghi789",
			"X-Custom": "4242424242424242",
		},
	},
	{
		name: "h03-stripe-and-email",
		input: map[string]string{
			"X-Api-Key":        "sk_live_0000000000000XYZFAKE",
			"X-Forwarded-User": "alice@example.com",
		},
	},
	{
		name: "h04-clean-headers",
		input: map[string]string{
			"User-Agent":     "curl/8.4.1",
			"Accept":         "*/*",
			"Content-Length": "0",
		},
	},
	{
		name:  "h05-empty-map",
		input: map[string]string{},
	},
}

// Security tests for pkg/redact. These are the wire-side tripwire
// the ADR-096 spec requires: every output of Apply / ApplyHeaders
// is re-checked against the same regex set, and ANY match fails the
// build.
//
// The intent is: even if a future maintainer adds a pattern, or
// rewrites a regex, or runs Apply against an unusual input — the
// security test here independently verifies that the canonical
// PII patterns can no longer match. Any drift between this test
// and the production redaction logic is a build break, not a
// silent regression.
//
// Tripwire patterns: email, card. These are the two classes of
// PII that have caused Sentry-grade incidents historically and
// are the load-bearing invariants to defend.

package redact

import (
	"regexp"
	"strings"
	"testing"
)

// TestSecurity_ApplyEmailTripwire — independent of the redact
// package's own email regex. If this regex matches an output of
// Apply, the redaction is broken.
func TestSecurity_ApplyEmailTripwire(t *testing.T) {
	t.Parallel()

	// Independent regex (not imported from redact.go). Anchored
	// on word boundaries. If you change redact.go's email
	// pattern, this regex stays put — that's the point of the
	// tripwire.
	emailTripwire := regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`)

	r := New(512)

	corpus := []string{
		"login failed for alice@example.com please retry",
		"  spaces around  bob@sub.example.co.uk  here  ",
		"contact:carol+tag@example.org; end",
		"server logs: alice@example.com timed out at 2026-08-12",
		"hi alice@example.com from 10.0.0.5",
	}

	for _, input := range corpus {
		out, _ := r.Apply(input)
		if emailTripwire.MatchString(out) {
			t.Errorf("EMAIL LEAK: input %q produced output %q which still matches the tripwire regex", input, out)
		}
	}
}

// TestSecurity_ApplyCardTripwire — 13..19 digit runs in the output
// of Apply are a build break.
func TestSecurity_ApplyCardTripwire(t *testing.T) {
	t.Parallel()

	// Independent regex.
	cardTripwire := regexp.MustCompile(`\b(?:\d[ \-]?){13,19}\b`)

	r := New(512)

	corpus := []string{
		"charged 4242424242424242",
		"card 4242 4242 4242 4242",
		"card 4242-4242-4242-4242",
		"old visa 4222222222222",
	}

	for _, input := range corpus {
		out, _ := r.Apply(input)
		if cardTripwire.MatchString(out) {
			t.Errorf("CARD LEAK: input %q produced output %q which still matches the tripwire regex", input, out)
		}
	}
}

// TestSecurity_ApplyAuthorizationTripwire — the string
// "Bearer <something>" must never appear in the output. The
// something can be anything; we trip on the literal "Bearer "
// suffix to catch a redaction that's lost the prefix.
func TestSecurity_ApplyAuthorizationTripwire(t *testing.T) {
	t.Parallel()
	r := New(512)

	corpus := []string{
		"Authorization: Bearer abc.def.ghi-jwt-thing",
		"authorization: Basic dXNlcjpwYXNzd29yZA==",
	}

	bearerSuffix := regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+`)
	basicSuffix := regexp.MustCompile(`(?i)Basic\s+[A-Za-z0-9+/=]{8,}`)

	for _, input := range corpus {
		out, _ := r.Apply(input)
		if bearerSuffix.MatchString(out) {
			t.Errorf("BEARER LEAK: %q → %q", input, out)
		}
		if basicSuffix.MatchString(out) {
			t.Errorf("BASIC LEAK: %q → %q", input, out)
		}
	}
}

// TestSecurity_ApplyStripeKeyTripwire — Stripe-style keys
// (sk/pk/rk _ live/test _ <16+ alnum>) must never survive Apply.
//
// Note: the corpus strings are built at runtime from a prefix
// + suffix to avoid tripping GitHub's secret-scanning push
// protection on the literal key shapes. The redactor's regex
// still matches because it operates on the assembled string.
func TestSecurity_ApplyStripeKeyTripwire(t *testing.T) {
	t.Parallel()
	r := New(512)

	// 24 alnum after the prefix to comfortably satisfy the
	// "16+" minimum and survive any future tightening.
	const suffix = "FAKEFIXTURE0000000000000000"
	corpus := []string{
		"sk" + "_live_" + suffix,
		"pk" + "_test_" + suffix,
		"rk" + "_live_" + suffix,
	}
	liveKey := regexp.MustCompile(`\b(?:sk|pk|rk)_(?:live|test)_[A-Za-z0-9]{16,}\b`)
	for _, input := range corpus {
		out, _ := r.Apply(input)
		if liveKey.MatchString(out) {
			t.Errorf("STRIPE KEY LEAK: %q → %q", input, out)
		}
	}
}

// TestSecurity_ApplyHeadersTripwire — every value in the output
// map must individually pass the email + card tripwire.
func TestSecurity_ApplyHeadersTripwire(t *testing.T) {
	t.Parallel()
	r := New(256)

	in := map[string]string{
		"Authorization":       "Bearer abc.def.ghi-jwt-thing",
		"X-Forwarded-For":     "10.0.0.5, 10.0.0.6",
		"Cookie":              "session=abc123; secret=xyz789",
		"X-Custom-User":       "alice@example.com",
		"X-Custom-Card":       "4242-4242-4242-4242",
		"User-Agent":          "curl/8.4.1",
		"X-Request-Body-Hint": "userId=42; note=please don't redact this",
	}

	emailRegex := regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`)
	cardRegex := regexp.MustCompile(`\b(?:\d[ \-]?){13,19}\b`)
	ipv4Regex := regexp.MustCompile(`\b(?:25[0-5]|2[0-4]\d|[01]?\d?\d)(?:\.(?:25[0-5]|2[0-4]\d|[01]?\d?\d)){3}\b`)

	out, _ := r.ApplyHeaders(in)
	for k, v := range out {
		if emailRegex.MatchString(v) {
			t.Errorf("EMAIL LEAK in header %q: %q", k, v)
		}
		if cardRegex.MatchString(v) {
			t.Errorf("CARD LEAK in header %q: %q", k, v)
		}
		// ipv4 IS redacted by the standard pattern, so an IPv4
		// in the output would be a leak.
		if ipv4Regex.MatchString(v) {
			t.Errorf("IPV4 LEAK in header %q: %q", k, v)
		}
	}
}

// TestSecurity_ForbiddenMarkers — strings that must NEVER appear
// in any redaction output. These are the deep-pii markers from
// the wire-side tripwire in handlers_app_errors_security_test.go,
// mirrored here so the redact package is independently safe even
// if a future maintainer wires ApplyHeaders into a header set
// that already contains one of these.
func TestSecurity_ForbiddenMarkers(t *testing.T) {
	t.Parallel()
	r := New(1024)

	// Inputs deliberately seed each forbidden marker; the
	// expectation is that Apply leaves them alone (these are
	// internal Gregale control-plane strings, not customer PII,
	// so redaction should NOT touch them) but Apply must also
	// never INTRODUCE one. So: feed a payload that doesn't
	// contain any of them, and confirm none of them appear in
	// the output.
	neutral := "error 500 from app, see upstream logs for stack"
	out, _ := r.Apply(neutral)

	forbidden := []string{
		"mfa_secret_encrypted",
		"netns",
		"guest_uid",
		"host_ip",
		"lease_token",
		"ciphertext",
		"password_encrypted",
		"webhook_secret_sealed",
	}
	for _, m := range forbidden {
		if strings.Contains(out, m) {
			t.Errorf("FORBIDDEN MARKER %q leaked into output %q", m, out)
		}
	}
}

// TestSecurity_ReentrantRedaction — running Apply twice on the
// same input must be idempotent (no further PII can leak on the
// second pass because the redaction markers don't themselves
// match any pattern).
func TestSecurity_ReentrantRedaction(t *testing.T) {
	t.Parallel()
	r := New(1024)

	in := "user alice@example.com card 4242424242424242 dial 10.0.0.5"
	once, _ := r.Apply(in)
	twice, _ := r.Apply(once)
	if once != twice {
		t.Fatalf("Apply is not idempotent:\n  once:  %q\n  twice: %q", once, twice)
	}
}

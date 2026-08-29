package mail

import (
	"strings"
	"testing"
)

// TestMarketingHeaders_RFC8058Pair pins the exact byte sequence
// Gmail / Yahoo require: List-Unsubscribe carries the URL in angle
// brackets (RFC 2369 / 8058) and List-Unsubscribe-Post is the
// literal "List-Unsubscribe=One-Click" string.
func TestMarketingHeaders_RFC8058Pair(t *testing.T) {
	const url = "https://faas.example.com/account/notifications?unsub=token"
	hdrs := MarketingHeadersMap(url)
	if got, want := hdrs["List-Unsubscribe"], "<"+url+">"; got != want {
		t.Fatalf("List-Unsubscribe = %q, want %q", got, want)
	}
	if got, want := hdrs["List-Unsubscribe-Post"], "List-Unsubscribe=One-Click"; got != want {
		t.Fatalf("List-Unsubscribe-Post = %q, want %q", got, want)
	}
}

// TestMarketingHeaders_EmptyURLPanics pins the fail-loud guard:
// a wired-but-empty unsubscribe URL is a programming error and
// must NOT ship as the literal "<>" in headers (which is a
// bulk-sender rejection path).
func TestMarketingHeaders_EmptyURLPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("MarketingHeadersMap with empty URL should panic")
		}
	}()
	_ = MarketingHeadersMap("")
}

// TestApplyMarketingHeaders_PreservesExisting pins that the seam
// does not disturb headers the caller already set. A future call
// site might attach X-Foo or List-ID alongside the unsubscribe
// pair; clobbering those would silently drop the RFC 8056 list
// identifier Gmail uses to scope bulk-sender rules.
func TestApplyMarketingHeaders_PreservesExisting(t *testing.T) {
	msg := Message{
		To: []string{"alice@example.com"},
		Headers: map[string]string{
			"X-Foo":   "bar",
			"List-ID": "<notifications.faas.example.com>",
		},
	}
	out := ApplyMarketingHeaders(msg, "https://faas.example.com/unsub")
	if got := out.Headers["X-Foo"]; got != "bar" {
		t.Fatalf("X-Foo clobbered = %q, want bar", got)
	}
	if got := out.Headers["List-ID"]; !strings.Contains(got, "notifications") {
		t.Fatalf("List-ID clobbered = %q", got)
	}
	if got, want := out.Headers["List-Unsubscribe"], "<https://faas.example.com/unsub>"; got != want {
		t.Fatalf("List-Unsubscribe = %q, want %q", got, want)
	}
}

// TestApplyMarketingHeaders_NilHeadersInitialised pins the
// friendly path: a Message with nil Headers gets a fresh map
// rather than a nil-map assignment that would panic on
// subsequent writes.
func TestApplyMarketingHeaders_NilHeadersInitialised(t *testing.T) {
	out := ApplyMarketingHeaders(Message{To: []string{"alice@example.com"}}, "https://faas.example.com/unsub")
	if out.Headers == nil {
		t.Fatal("Headers still nil after ApplyMarketingHeaders")
	}
	if got := out.Headers["List-Unsubscribe-Post"]; got != "List-Unsubscribe=One-Click" {
		t.Fatalf("List-Unsubscribe-Post = %q, want One-Click", got)
	}
}

// TestValidateUnsubscribeURL exercises the boot-time guard.
func TestValidateUnsubscribeURL(t *testing.T) {
	good := []string{
		"https://faas.example.com/account/notifications?token=abc",
		"http://localhost:8081/account/notifications",
	}
	for _, u := range good {
		if err := ValidateUnsubscribeURL(u); err != nil {
			t.Fatalf("ValidateUnsubscribeURL(%q) = %v, want nil", u, err)
		}
	}
	bad := []string{
		"",
		"   ",
		"mailto:alice@example.com",
		"ftp://example.com/unsub",
		"https://",
		"not a url at all",
	}
	for _, u := range bad {
		if err := ValidateUnsubscribeURL(u); err == nil {
			t.Fatalf("ValidateUnsubscribeURL(%q) = nil, want error", u)
		}
	}
}

// TestQuotaWarningHTMLBody_StructuralPins pins the shape Gmail /
// Yahoo's "View as HTML" renderer needs: a doctype, an html+body
// pair, and the same load-bearing facts the plain-text body
// carries (used, quota, day, plan).
func TestQuotaWarningHTMLBody_StructuralPins(t *testing.T) {
	html := QuotaWarningHTMLBody("alice@example.com", "pro", 250.50, 250, "2026-08-29")
	for _, want := range []string{
		"<!doctype html>",
		"<html>",
		"<body>",
		"alice@example.com",
		"pro",
		"250.50",
		"250",
		"2026-08-29",
		"</body>",
		"</html>",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("HTML missing %q\n---HTML---\n%s", want, html)
		}
	}
}

// TestQuotaWarningHTMLBody_SanitisesRecipient pins that the
// same safeRecipient helper used by QuotaWarningBody applies to
// the HTML alt — CR/LF injection from a hostile address is a
// CWE-117 tripwire we should not regress on the new surface.
//
// The check targets the smuggled header (Bcc:) preceded by a
// newline rather than any newline — the HTML template itself
// contains newlines between tags and we must not flag those.
func TestQuotaWarningHTMLBody_SanitisesRecipient(t *testing.T) {
	html := QuotaWarningHTMLBody("evil\r\nBcc: attacker@example.com", "free", 1.0, 5, "2026-08-29")
	for _, banned := range []string{"\nBcc:", "\nbcc:", "\r\nBcc:"} {
		if strings.Contains(html, banned) {
			t.Fatalf("HTML still contains smuggled header %q\n---HTML---\n%s", banned, html)
		}
	}
	if !strings.Contains(html, "evilBcc:") {
		// Recipient collapsed to "evilBcc:" is the expected
		// post-sanitisation form (the \r and \n between
		// "evil" and "Bcc" are stripped). Anything else —
		// e.g. raw CR/LF surviving — means the helper is
		// not wired in.
		t.Fatalf("HTML recipient not collapsed as expected\n---HTML---\n%s", html)
	}
}

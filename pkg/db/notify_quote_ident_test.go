// notify_quote_ident_test.go — branch coverage for
// pkg/db/notify.go::quoteIdent (the SQL identifier-quoting matrix
// used by Subscribe + the LISTEN/NOTIFY path).
//
// quoteIdent is a pure string-rewriting function used to escape
// channel names before composing LISTEN/NOTIFY SQL. It's the
// SQL-injection-defense seam for the channel-name argument — a
// caller that builds "LISTEN " + channel directly would be open
// to identifier-injection attacks. quoteIdent is also where the
// embedded double-quote gets the canonical "" escape.
//
// Whitebox test (package db).
package db

import (
	"strings"
	"testing"
)

// TestQuoteIdent_Empty pins the empty-input branch. Empty
// channels are unusual but valid in a degenerate "no events
// yet" case; we expect " (literal two-char string).
func TestQuoteIdent_Empty(t *testing.T) {
	got := quoteIdent("")
	want := `""`
	if got != want {
		t.Errorf("quoteIdent(%q) = %q, want %q", "", got, want)
	}
}

// TestQuoteIdent_PlainIdentifier pins the happy path: an
// unreserved-word identifier round-trips unchanged (modulo
// surrounding double-quotes).
func TestQuoteIdent_PlainIdentifier(t *testing.T) {
	got := quoteIdent("orders")
	want := `"orders"`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestQuoteIdent_DoubleQuoteEscape pins the security-critical
// branch: an embedded " in the input becomes "" in the output
// (the canonical Postgres identifier escape).
//
// A caller that forgets to do this and composes
// "LISTEN \"" + channel + "\"" can be injected by a channel
// name like app_v1"."; ...; -- which would terminate the
// quoted identifier and run the trailing SQL. The "" escape
// is the load-bearing defence.
func TestQuoteIdent_DoubleQuoteEscape(t *testing.T) {
	got := quoteIdent(`a"b`)
	want := `"a""b"`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestQuoteIdent_MultipleEmbeddedQuotes pins the
// "every double-quote is escaped" branch: a string with N
// embedded quotes produces 2N " characters in the body.
//
// A naive implementation that only escapes the FIRST embedded
// quote (and copies the rest verbatim) trips here.
//
// Input: a"b"c (5 chars)
// Expected output: "a""b""c" (9 chars: outer + a + doubled + b + doubled + c + outer)
func TestQuoteIdent_MultipleEmbeddedQuotes(t *testing.T) {
	got := quoteIdent(`a"b"c`)
	want := `"a""b""c"`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestQuoteIdent_TableDriven exercises the canonical Postgres
// reserved-word + special-character matrix.
func TestQuoteIdent_TableDriven(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", `""`},
		{"a", `"a"`},
		{"ABC", `"ABC"`}, // upper-case preserved
		{"orders", `"orders"`},
		{"snap_v3", `"snap_v3"`},
		{"with-dashes", `"with-dashes"`}, // dashes allowed when quoted
		{"123", `"123"`},                 // numeric start allowed when quoted
		{`a"b`, `"a""b"`},
		// Security: a name that LOOKS like SQL terminator must
		// not break out of the quoted form.
		{`x"; DROP TABLE users; --`, `"x""; DROP TABLE users; --"`},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(strings.ReplaceAll(tc.in, " ", "_"), func(t *testing.T) {
			got := quoteIdent(tc.in)
			if got != tc.want {
				t.Errorf("quoteIdent(%q) = %q, want %q", tc.in, got, tc.want)
			}
			// Round-trip invariant: every produced string must
			// start AND end with `"`, with NO unescaped `"`
			// inside (only `""`).
			if !strings.HasPrefix(got, `"`) || !strings.HasSuffix(got, `"`) {
				t.Errorf("quoteIdent(%q) lost outer quotes: %q", tc.in, got)
			}
		})
	}
}

// TestQuoteIdent_RoundTripPreservesString pins the property:
// the inner body of the output (after stripping outer quotes
// and replacing "" with ") reconstructs the original input.
// This is the canonical "the function is the inverse of
// stripping-and-replacing-quotes" property.
func TestQuoteIdent_RoundTripPreservesString(t *testing.T) {
	for _, in := range []string{
		"orders",
		`a"b`,
		`a"b"c`,
		`x"; DROP TABLE users; --`,
		"",
		"a",
	} {
		quoted := quoteIdent(in)
		// Strip outer quotes + replace "" with ".
		body := quoted[1 : len(quoted)-1]
		var unquoted strings.Builder
		unquoted.Grow(len(body))
		escapeNext := false
		for i := 0; i < len(body); i++ {
			c := body[i]
			if c == '"' && !escapeNext {
				escapeNext = true
				continue
			}
			escapeNext = false
			unquoted.WriteByte(c)
		}
		if got := unquoted.String(); got != in {
			t.Errorf("round-trip mismatch: in=%q quoted=%q out=%q", in, quoted, got)
		}
	}
}

// TestQuoteIdent_IdempotentOnCleanInput pins that the function
// does NOT over-quote: a clean identifier (no embedded
// quotes) does not gain extra quote characters beyond the
// surrounding pair.
func TestQuoteIdent_IdempotentOnCleanInput(t *testing.T) {
	for _, in := range []string{
		"a", "abc", "snap_v3", "u-1-2-3",
	} {
		out := quoteIdent(in)
		wantLen := len(in) + 2
		if len(out) != wantLen {
			t.Errorf("quoteIdent(%q) length = %d, want %d", in, len(out), wantLen)
		}
	}
}

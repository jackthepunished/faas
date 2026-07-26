package logsanitize

import (
	"errors"
	"strings"
	"testing"
)

// TestField locks the contract for CodeQL's go/log-injection (CWE-117)
// defense. The helper strips ASCII control characters so a malicious
// producer cannot forge log lines via CR/LF injection; tabs survive
// because slog's JSON encoder and most log readers treat them as
// benign whitespace. Tests live with the implementation so a future
// refactor that drops the contract fails in the obvious place.
func TestField(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty stays empty", "", ""},
		{"plain passes through", "hello-world_42.app", "hello-world_42.app"},
		{"tab survives", "a\tb", "a\tb"},
		{"newline replaced", "a\nb", "a·b"},
		{"carriage return replaced", "a\rb", "a·b"},
		{"null replaced", "a\x00b", "a·b"},
		{"del replaced", "a\x7fb", "a·b"},
		{"unicode passes through", "héllo·世界", "héllo·世界"},
		{"multiple controls", "x\ny\rz\x00", "x·y·z·"},
		{"only controls", "\n\r\x00", "···"},
		{"path-style survives", "/v1/synthesize?foo=bar", "/v1/synthesize?foo=bar"},
		{"quoted path survives", `/foo/"bar"`, `/foo/"bar"`},
		{"multi-byte UTF-8 control-free survives", "日本-アプリ", "日本-アプリ"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Field(tc.in)
			if got != tc.want {
				t.Errorf("Field(%q) = %q, want %q", tc.in, got, tc.want)
			}
			// Belt + suspenders: no CR or LF ever leaves the helper.
			if strings.ContainsAny(got, "\r\n") {
				t.Errorf("Field(%q) leaked CR/LF: %q", tc.in, got)
			}
		})
	}
}

// TestFieldAny locks the interface{} companion contract for the
// recovery middleware and any other call site that logs an
// attacker-controllable any. Same defense as TestField but mapped
// across the four top-level branches of FieldAny.
func TestFieldAny(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		// nil → ""; covers panic(nil) being a legitimate recover() return.
		{"nil returns empty", nil, ""},
		// string delegates to Field().
		{"string delegates", "hello-world", "hello-world"},
		{"string still strips", "a\nb", "a·b"},
		// error: length-only prefix; sanitized message body.
		{"error clean", errors.New("boom"), "4-byte-error:boom"},
		{"error with CR/LF stripped",
			errors.New("bad\rinput\nhere"), "14-byte-error:bad·input·here"},
		{"nil error returns empty",
			error(nil), ""}, //nolint:gocritic // testing the (error)(nil) branch deliberately
		// anything else → fmt.Sprint then Field. fmt.Sprint on a
		// Stringer returns the String() output verbatim (not the
		// "%T(%v)" form); the hostile type asserts the bytes are
		// sanitized, not the framing.
		{"int goes through Sprint", 42, "42"},
		{"int negative", -7, "-7"},
		{"type with hostile String method",
			hostileStringer("evil\nlog\rline"), "evil·log·line"},
		// Default branch: non-stringer types route through fmt.Sprint
		// (struct framing) and then Field. A plain struct is the
		// simplest "not a Stringer, not a string, not an error, not
		// nil" case the type switch reaches.
		{"non-stringer struct goes through Sprint",
			struct{ X int }{42}, "{42}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FieldAny(tc.in)
			if got != tc.want {
				t.Errorf("FieldAny(%v) = %q, want %q", tc.in, got, tc.want)
			}
			if strings.ContainsAny(got, "\r\n") {
				t.Errorf("FieldAny(%v) leaked CR/LF: %q", tc.in, got)
			}
		})
	}
}

// hostileStringer is a test type whose String() deliberately contains
// CR/LF so a hostile recovery value can't smuggle newlines through
// slog's Any() path. FieldAny must route through Field() and strip.
type hostileStringer string

func (h hostileStringer) String() string { return string(h) }

// TestRedactValue locks the length-only placeholder contract that
// apid's secrets handlers depend on so audit lines can carry a length
// signal without ever leaking the value. The only output shape is
// "<redacted:N>" where N == len(s); the input never appears in the
// output. Covers the empty-string branch (length 0) and the itoa
// path for 1-, 2-, 3-, 4-, and 5-digit lengths up to the documented
// Scale-plan secret ceiling (32 KB).
func TestRedactValue(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty stays at zero", "", "<redacted:0>"},
		{"single byte", "x", "<redacted:1>"},
		{"short ascii", "abcdef", "<redacted:6>"},
		{"two digit length", "hello world!", "<redacted:12>"},
		{"three digit length", strings.Repeat("a", 100), "<redacted:100>"},
		{"four digit length", strings.Repeat("a", 9999), "<redacted:9999>"},
		// 32768 == Limits.SecretValueMaxBytes at the Scale plan; the
		// formatted length stays well inside the 20-byte itoa buffer.
		{"scale plan secret ceiling",
			strings.Repeat("a", 32768), "<redacted:32768>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactValue(tc.in)
			if got != tc.want {
				t.Errorf("RedactValue(len=%d) = %q, want %q",
					len(tc.in), got, tc.want)
			}
			// Belt + suspenders: the secret value must never appear
			// in the placeholder. A regression that returns the
			// input by mistake would fail here, not just on the
			// exact-match check above (which would mask it for the
			// empty input).
			if tc.in != "" && strings.Contains(got, tc.in) {
				t.Errorf("RedactValue leaked input: got %q contains %q",
					got, tc.in)
			}
			if !strings.HasPrefix(got, "<redacted:") ||
				!strings.HasSuffix(got, ">") {
				t.Errorf("RedactValue shape = %q (want <redacted:N>)", got)
			}
		})
	}
}

// TestItoa locks the contract of the internal base-10 formatter. The
// function is only ever called with len(s) ≤ 32 KB so non-negative
// inputs are the contract; we don't assert anything about negatives
// (the function's behaviour on negative inputs is not part of the
// API).
func TestItoa(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "0"},
		{1, "1"},
		{9, "9"},
		{10, "10"},
		{123, "123"},
		{32768, "32768"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			if got := itoa(tc.n); got != tc.want {
				t.Errorf("itoa(%d) = %q, want %q", tc.n, got, tc.want)
			}
		})
	}
}

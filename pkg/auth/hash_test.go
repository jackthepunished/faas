package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestHashEmail_NormalisesCaseAndWhitespace(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "lowercase",
			in:   "alice@example.com",
			want: sha256Hex("alice@example.com"),
		},
		{
			name: "uppercase local",
			in:   "Alice@Example.com",
			want: sha256Hex("alice@example.com"),
		},
		{
			name: "leading whitespace",
			in:   "   alice@example.com",
			want: sha256Hex("alice@example.com"),
		},
		{
			name: "trailing whitespace",
			in:   "alice@example.com\t",
			want: sha256Hex("alice@example.com"),
		},
		{
			name: "all whitespace collapsed",
			in:   "  Alice@Example.com  \n",
			want: sha256Hex("alice@example.com"),
		},
		{
			name: "empty",
			in:   "",
			want: sha256Hex(""),
		},
		{
			name: "whitespace only",
			in:   "   \t\n",
			want: sha256Hex(""),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := HashEmail(tc.in)
			if got != tc.want {
				t.Errorf("HashEmail(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestHashEmail_DeterministicAcrossCalls(t *testing.T) {
	// The same email must hash to the same value across calls so the
	// audit row can join across subsystems (login failures from /login
	// and /signup must share the same email_hash key).
	a := HashEmail("victim@example.com")
	b := HashEmail("VICTIM@example.com")
	c := HashEmail(" victim@example.com ")
	if a != b || b != c {
		t.Errorf("HashEmail not deterministic: a=%q b=%q c=%q", a, b, c)
	}
}

func TestHashEmail_DoesNotReturnPlaintext(t *testing.T) {
	// Belt-and-braces: confirm the audit row will not leak the literal
	// email. This is the contract that the SOC 2 evidence chain relies
	// on.
	in := "alice@example.com"
	got := HashEmail(in)
	if strings.Contains(got, "alice") || strings.Contains(got, "example") {
		t.Errorf("HashEmail leaked plaintext component: got=%q", got)
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

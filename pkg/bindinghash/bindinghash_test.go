package bindinghash

import (
	"strings"
	"testing"
)

func TestCompute_EmptyInputsReturnEmptyString(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	keyFn := func() []byte { return key }
	if got := Compute("", "chrome", keyFn); got != "" {
		t.Errorf("Compute(\"\", chrome) = %q, want \"\"", got)
	}
	if got := Compute("1.2.3.4", "", keyFn); got != "" {
		t.Errorf("Compute(1.2.3.4, \"\") = %q, want \"\"", got)
	}
	if got := Compute("", "", keyFn); got != "" {
		t.Errorf("Compute(\"\", \"\") = %q, want \"\"", got)
	}
}

func TestCompute_NilKeyReturnsEmptyString(t *testing.T) {
	if got := Compute("1.2.3.4", "chrome", nil); got != "" {
		t.Errorf("Compute with nil keyFn = %q, want \"\"", got)
	}
	keyFn := func() []byte { return nil }
	if got := Compute("1.2.3.4", "chrome", keyFn); got != "" {
		t.Errorf("Compute with nil-key keyFn = %q, want \"\"", got)
	}
}

func TestCompute_HMACDeterministic(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	keyFn := func() []byte { return key }
	a := Compute("1.2.3.4", "chrome", keyFn)
	b := Compute("1.2.3.4", "chrome", keyFn)
	if a == "" {
		t.Fatalf("Compute returned empty for non-empty inputs")
	}
	if a != b {
		t.Errorf("Compute not deterministic: %q vs %q", a, b)
	}
	if len(a) != 64 {
		// hex(SHA256) = 64 chars
		t.Errorf("Compute returned %d chars, want 64", len(a))
	}
}

func TestCompute_DifferentInputsDifferentHash(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	keyFn := func() []byte { return key }
	a := Compute("1.2.3.4", "chrome", keyFn)
	b := Compute("1.2.3.5", "chrome", keyFn)  // IP differs
	c := Compute("1.2.3.4", "firefox", keyFn) // UA family differs
	if a == b {
		t.Errorf("IP-only change should produce different hash")
	}
	if a == c {
		t.Errorf("UA-family-only change should produce different hash")
	}
}

func TestCompute_DifferentKeysDifferentHash(t *testing.T) {
	// Confirms the HMAC is keyed — a different key should produce a
	// different hash for the same IP+UA. This is the load-bearing
	// property that distinguishes HMAC from bare SHA-256.
	k1 := []byte("0123456789abcdef0123456789abcdef")
	k2 := []byte("fedcba9876543210fedcba9876543210")
	a := Compute("1.2.3.4", "chrome", func() []byte { return k1 })
	b := Compute("1.2.3.4", "chrome", func() []byte { return k2 })
	if a == "" || b == "" {
		t.Fatalf("Compute returned empty: a=%q b=%q", a, b)
	}
	if a == b {
		t.Errorf("HMAC must be keyed: a=%q b=%q", a, b)
	}
}

func TestUAFamily_Buckets(t *testing.T) {
	cases := []struct {
		ua   string
		want string
	}{
		{"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/123.0.0.0 Safari/537.36", "chrome"},
		{"Mozilla/5.0 (X11; Linux x86_64; rv:124.0) Gecko/20100101 Firefox/124.0", "firefox"},
		{"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_4) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Safari/605.1.15", "safari"},
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/123.0.0.0 Safari/537.36 Edg/123.0.0.0", "edge"},
		{"curl/8.4.0", "curl"},
		{"Wget/1.21.4", "wget"},
		{"faas-cli/1.2.3 (linux; go1.23)", "cli"},
		{"", "unknown"},
		{"totally custom", "unknown"},
	}
	for _, c := range cases {
		got := UAFamily(c.ua)
		if got != c.want {
			t.Errorf("UAFamily(%q) = %q, want %q", c.ua, got, c.want)
		}
	}
}

func TestUAFamily_CaseInsensitive(t *testing.T) {
	// The family classifier is case-insensitive — clients in the
	// wild occasionally capitalise "Mozilla" or "Safari".
	in := "MOZILLA/5.0 (X11; LINUX X86_64) APPLEWEBKIT/537.36 (KHTML, LIKE GECKO) CHROME/123.0.0.0 SAFARI/537.36"
	if got := UAFamily(in); !strings.HasPrefix(got, "chrome") {
		t.Errorf("UAFamily(case-insensitive) = %q, want chrome", got)
	}
}

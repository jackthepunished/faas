// Whitebox tests for ScanFile. Mirrors the existing scan_test.go shape:
// each case names the input bytes + expected Finding slice.
package secretscan

import (
	"testing"
)

// fakeStripeLiveKey is declared in scan_test.go (the canonical fixture);
// we reuse it here for the JSON-quoted-key positive case.

func TestScanFile_Positives(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		contents string
		want     []Finding
	}{
		{
			name:     "pem_armour",
			path:     "keys/server.pem",
			contents: "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEAxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\n-----END RSA PRIVATE KEY-----\n",
			want: []Finding{
				{File: "keys/server.pem", Line: 1, Provider: "private_key_block", Severity: SeverityHigh, Snippet: "-----B…----"},
			},
		},
		{
			name:     "aws_in_yaml",
			path:     "config/prod.yaml",
			contents: "aws_access_key_id: AKIAIOSFODNN7EXAMPLE\n",
			// KeyHint for aws_access is "aws"; "aws_access_key_id" contains it.
			want: []Finding{
				{File: "config/prod.yaml", Line: 1, Key: "aws_access_key_id", Provider: "aws_access", Severity: SeverityHigh, Snippet: "AKIAIO…MPLE"},
			},
		},
		{
			name:     "github_pat_in_ts",
			path:     "src/index.ts",
			contents: "const GITHUB_TOKEN = \"ghp_aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789\"\n",
			// KeyHint for github_pat is "github"; "GITHUB_TOKEN" contains it.
			want: []Finding{
				{File: "src/index.ts", Line: 1, Key: "GITHUB_TOKEN", Provider: "github_pat", Severity: SeverityHigh, Snippet: "ghp_aB…6789"},
			},
		},
		{
			name:     "json_quoted_key",
			path:     "config.json",
			contents: `{"STRIPE_SECRET_KEY": "` + fakeStripeLiveKey + `"}` + "\n",
			// The leading/trailing quotes are stripped from the key.
			// Snippet policy matches the env path: first 6 + ellipsis + last 4.
			want: []Finding{
				{File: "config.json", Line: 1, Key: "STRIPE_SECRET_KEY", Provider: "stripe_live", Severity: SeverityHigh, Snippet: "sk_liv…XXXX"},
			},
		},
		{
			name:     "clean_python",
			path:     "main.py",
			contents: "import os\n\ndef hello():\n    print('hello world')\n",
			want:     nil,
		},
		{
			name:     "comment_line_with_looking_key",
			path:     "notes.md",
			contents: "# STRIPE_SECRET_KEY=" + fakeStripeLiveKey + "\n",
			// Comments are skipped, not scanned.
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ScanFile(tc.path, []byte(tc.contents))
			if len(got) != len(tc.want) {
				t.Fatalf("ScanFile(%q) returned %d findings, want %d: %+v", tc.contents, len(got), len(tc.want), got)
			}
			for i, f := range got {
				if f != tc.want[i] {
					t.Errorf("finding[%d] = %+v, want %+v", i, f, tc.want[i])
				}
			}
		})
	}
}

func TestScanFile_LineNumbers(t *testing.T) {
	contents := "import os\n\n# blank line follows\nconst K = \"ghp_aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789\"\n"
	got := ScanFile("main.go", []byte(contents))
	if len(got) != 1 {
		t.Fatalf("want 1 finding, got %d: %+v", len(got), got)
	}
	if got[0].Line != 4 {
		t.Errorf("Line = %d, want 4 (import at 1, blank at 2, comment at 3, secret at 4)", got[0].Line)
	}
}

func TestScanFile_NoRawValueInSnippet(t *testing.T) {
	// Privacy guard: snippet must never contain the full raw value.
	contents := "STRIPE_SECRET_KEY=" + fakeStripeLiveKey + "\n"
	got := ScanFile(".env", []byte(contents))
	if len(got) != 1 {
		t.Fatalf("want 1 finding, got %d", len(got))
	}
	if got[0].Snippet == fakeStripeLiveKey {
		t.Errorf("Snippet leaked the full raw value: %q", got[0].Snippet)
	}
}

func TestIsKeyShaped(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"STRIPE_SECRET_KEY", true},
		{"_PRIVATE", true},
		{"api-key", true},
		{"stripe.key", true},
		{"123foo", false}, // can't start with digit
		{"foo bar", false},
		{"", false},
		{"foo;rm", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := isKeyShaped([]byte(tc.in)); got != tc.want {
				t.Errorf("isKeyShaped(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

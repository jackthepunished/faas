package main

import (
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// silentEgressLogger discards all log output so the bundle
// tests don't pollute the test binary's stderr. Errors are
// surfaced by the function's error return, not by slog.Warn.
func silentEgressLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func mustPrefix(t *testing.T, s string) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return p
}

func TestLoadEgressBundle_Valid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "operator_allowlist.toml")
	content := `# Issue #679 / PR-A — operator egress bundle.
cidrs = [
    "203.0.113.0/24",
    "198.51.100.0/24",
    "2606:4700::/32",
    "192.0.2.0/24",
]
`
	if err := os.WriteFile(path, []byte(content), 0o400); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := LoadEgressBundle(path, silentEgressLogger())
	if err != nil {
		t.Fatalf("LoadEgressBundle: %v", err)
	}
	if len(got.CIDRs) != 4 {
		t.Fatalf("CIDRs len = %d, want 4; got=%v", len(got.CIDRs), got.CIDRs)
	}
	// sorted ascending by .String()
	want := []netip.Prefix{
		mustPrefix(t, "192.0.2.0/24"),
		mustPrefix(t, "198.51.100.0/24"),
		mustPrefix(t, "203.0.113.0/24"),
		mustPrefix(t, "2606:4700::/32"),
	}
	for i, p := range got.CIDRs {
		if p != want[i] {
			t.Errorf("CIDRs[%d] = %s, want %s", i, p, want[i])
		}
	}
}

func TestLoadEgressBundle_MissingFileIsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.toml")
	got, err := LoadEgressBundle(path, silentEgressLogger())
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if len(got.CIDRs) != 0 {
		t.Errorf("CIDRs = %v, want empty", got.CIDRs)
	}
}

func TestLoadEgressBundle_EmptyFileIsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.toml")
	if err := os.WriteFile(path, []byte{}, 0o400); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := LoadEgressBundle(path, silentEgressLogger())
	if err != nil {
		t.Fatalf("empty file should not error: %v", err)
	}
	if len(got.CIDRs) != 0 {
		t.Errorf("CIDRs = %v, want empty", got.CIDRs)
	}
}

func TestLoadEgressBundle_MalformedTomlReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.toml")
	if err := os.WriteFile(path, []byte("this is = not [valid toml"), 0o400); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := LoadEgressBundle(path, silentEgressLogger())
	if err == nil {
		t.Fatal("malformed TOML should return error")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("error = %v, want parse-failure", err)
	}
}

func TestLoadEgressBundle_PerEntryParseErrorDrops(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mixed.toml")
	content := `cidrs = [
    "203.0.113.0/24",
    "not-a-cidr",
    "198.51.100.0/24",
]
`
	if err := os.WriteFile(path, []byte(content), 0o400); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := LoadEgressBundle(path, silentEgressLogger())
	if err != nil {
		t.Fatalf("LoadEgressBundle: %v", err)
	}
	if len(got.CIDRs) != 2 {
		t.Fatalf("CIDRs len = %d, want 2 (bad entry dropped, others survive); got=%v", len(got.CIDRs), got.CIDRs)
	}
	if got.CIDRs[0] != mustPrefix(t, "198.51.100.0/24") {
		t.Errorf("CIDRs[0] = %s, want 198.51.100.0/24", got.CIDRs[0])
	}
	if got.CIDRs[1] != mustPrefix(t, "203.0.113.0/24") {
		t.Errorf("CIDRs[1] = %s, want 203.0.113.0/24", got.CIDRs[1])
	}
}

func TestLoadEgressBundle_RejectsZeroMask(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "zero.toml")
	content := `cidrs = [
    "203.0.113.0/24",
    "0.0.0.0/0",
    "::/0",
    "198.51.100.0/24",
]
`
	if err := os.WriteFile(path, []byte(content), 0o400); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := LoadEgressBundle(path, silentEgressLogger())
	if err != nil {
		t.Fatalf("LoadEgressBundle: %v", err)
	}
	if len(got.CIDRs) != 2 {
		t.Fatalf("CIDRs len = %d, want 2 (both /0 entries dropped); got=%v", len(got.CIDRs), got.CIDRs)
	}
	if got.CIDRs[0] != mustPrefix(t, "198.51.100.0/24") {
		t.Errorf("CIDRs[0] = %s, want 198.51.100.0/24", got.CIDRs[0])
	}
	if got.CIDRs[1] != mustPrefix(t, "203.0.113.0/24") {
		t.Errorf("CIDRs[1] = %s, want 203.0.113.0/24", got.CIDRs[1])
	}
}

func TestLoadEgressBundle_Dedupes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dup.toml")
	content := `cidrs = [
    "203.0.113.0/24",
    "203.0.113.0/24",
    "198.51.100.0/24",
    "203.0.113.5/24",
]
`
	if err := os.WriteFile(path, []byte(content), 0o400); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := LoadEgressBundle(path, silentEgressLogger())
	if err != nil {
		t.Fatalf("LoadEgressBundle: %v", err)
	}
	// 203.0.113.0/24 == 203.0.113.5/24 after .String() canonicalisation? No — they
	// have different addresses. So we expect 3 entries:
	// 198.51.100.0/24, 203.0.113.0/24, 203.0.113.5/24.
	if len(got.CIDRs) != 3 {
		t.Fatalf("CIDRs len = %d, want 3 (one exact dup dropped); got=%v", len(got.CIDRs), got.CIDRs)
	}
	want := []netip.Prefix{
		mustPrefix(t, "198.51.100.0/24"),
		mustPrefix(t, "203.0.113.0/24"),
		mustPrefix(t, "203.0.113.5/24"),
	}
	for i, p := range got.CIDRs {
		if p != want[i] {
			t.Errorf("CIDRs[%d] = %s, want %s", i, p, want[i])
		}
	}
}

func TestLoadEgressBundle_AllRejectsGivesEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "allbad.toml")
	content := `cidrs = [
    "0.0.0.0/0",
    "not-a-cidr",
    "::/0",
]
`
	if err := os.WriteFile(path, []byte(content), 0o400); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := LoadEgressBundle(path, silentEgressLogger())
	if err != nil {
		t.Fatalf("all-rejected should not error: %v", err)
	}
	if len(got.CIDRs) != 0 {
		t.Errorf("CIDRs = %v, want empty (all entries rejected)", got.CIDRs)
	}
}

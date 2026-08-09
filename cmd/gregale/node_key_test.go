// Tests for the operator-side per-node CapacityReport signing
// keypair CLI (`gregale node-key init|rotate|status`).
//
// Mirrors sign_keys_test.go's surface (commands_sign_keys.go is the
// upstream pattern from PR #371 / ADR-038). The differences worth
// pinning:
//
//   - The priv key is mode 0400 root:root (NOT 0440 root:gregale —
//     vmmd is the only root daemon per CLAUDE.md §11, so the
//     canonical install is owner-only). cmd/vmmd/main.go::loadNodeSigningKey
//     is strict on 0o400 and refuses anything looser; the CLI must
//     match so the operator doesn't write a key vmmd then refuses
//     to load.
//   - The init path prints the key_id (SHA-256 hex of the SPKI) so
//     the operator can confirm at a glance that the freshly-
//     generated key matches the value schedd's NodeKeyRegistry
//     will index against.
//   - Status reports BOTH files' mode + sha256[:12] fingerprint;
//     missing files print an explicit "missing" line and the
//     command returns 0 so the operator sees both paths even if
//     one is absent.
//
// All I/O is local (t.TempDir + the osStdout package seam) — no
// httptest, no authedClient, no SDK round-trip — because
// operator-side keypair provisioning never hits apid. This matches
// the sign-keys test surface (sign_keys_test.go:13-20).
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCmdNodeKey_NoArgs_PrintsUsage pins the parent-dispatcher
// contract: zero args returns 1 and prints a usage line to stderr.
// Mirrors TestCmdSignKeys_NoArgs_PrintsUsage.
func TestCmdNodeKey_NoArgs_PrintsUsage(t *testing.T) {
	_, readStderr, restore := swapIO(t)
	defer restore()

	if code := cmdNodeKey(nil); code != 1 {
		t.Errorf("cmdNodeKey(nil) = %d, want 1", code)
	}
	got := readStderr()
	if !strings.Contains(got, "usage: gregale node-key") {
		t.Errorf("stderr = %q, want usage line", got)
	}
	if !strings.Contains(got, "init|rotate|status") {
		t.Errorf("stderr = %q, want leaf enumeration", got)
	}
}

// TestCmdNodeKey_UnknownSubcommand pins the rejection path. Mirrors
// TestCmdSignKeys_UnknownSubcommand — unknown leaf returns 1 with
// "unknown subcommand" + leaf enumeration on stderr.
func TestCmdNodeKey_UnknownSubcommand(t *testing.T) {
	_, readStderr, restore := swapIO(t)
	defer restore()

	if code := cmdNodeKey([]string{"bogus"}); code != 1 {
		t.Errorf("cmdNodeKey(bogus) = %d, want 1", code)
	}
	if !strings.Contains(readStderr(), "unknown subcommand") {
		t.Errorf("stderr = %q, want 'unknown subcommand'", readStderr())
	}
	if !strings.Contains(readStderr(), "init, rotate, status") {
		t.Errorf("stderr = %q, want leaf enumeration", readStderr())
	}
}

// TestCmdNodeKeyInit_WritesBothFilesWithCanonicalModes pins the
// load-bearing contract: the priv file is mode 0400 root:root
// (matching cmd/vmmd/main.go::loadNodeSigningKey's strict 0o400
// check), the pub file is mode 0444, and the PEM block type is
// PRIVATE KEY (PKCS#8) — the shape loadNodeSigningKey parses.
func TestCmdNodeKeyInit_WritesBothFilesWithCanonicalModes(t *testing.T) {
	out, restore := swapStdout(t)
	defer restore()

	dir := t.TempDir()
	privPath := filepath.Join(dir, "node.key")
	pubPath := filepath.Join(dir, "node.pub")

	if code := cmdNodeKeyInit([]string{"--node-key", privPath, "--node-key-pub", pubPath}); code != 0 {
		t.Fatalf("cmdNodeKeyInit = %d, want 0", code)
	}

	privInfo, err := os.Stat(privPath)
	if err != nil {
		t.Fatalf("stat priv: %v", err)
	}
	// 0400 strict — vmmd is the only root daemon, so the canonical
	// install is owner-only. cmd/vmmd/main.go:151 enforces this on
	// the read side; the CLI must match on the write side.
	if got := privInfo.Mode().Perm(); got != 0o400 {
		t.Errorf("priv mode = %#o, want 0o400", got)
	}
	// Reject setuid/setgid/sticky bits too — loadNodeSigningKey
	// refuses those even if perm == 0o400. Mirror the check here so
	// the operator's "init ran cleanly" doesn't lie.
	if extra := privInfo.Mode() &^ os.ModePerm; extra != 0 {
		t.Errorf("priv has setuid/setgid/sticky bits (%#o)", extra)
	}

	pubInfo, err := os.Stat(pubPath)
	if err != nil {
		t.Fatalf("stat pub: %v", err)
	}
	if got := pubInfo.Mode().Perm(); got != 0o444 {
		t.Errorf("pub mode = %#o, want 0o444", got)
	}

	// PEM shape — loadNodeSigningKey's `if block.Type != "PRIVATE KEY"`
	// check at cmd/vmmd/main.go:170 means a SEC1 EC PRIVATE KEY block
	// would silently fail at boot. Pin the type here.
	privBytes, err := os.ReadFile(privPath)
	if err != nil {
		t.Fatalf("read priv: %v", err)
	}
	if !strings.Contains(string(privBytes), "PRIVATE KEY") {
		t.Errorf("priv file missing PEM PRIVATE KEY block")
	}
	// Should NOT be SEC1 — the canonical install is PKCS#8.
	if strings.Contains(string(privBytes), "EC PRIVATE KEY") {
		t.Errorf("priv file is SEC1; want PKCS#8 (block type PRIVATE KEY)")
	}

	// And the init path must print the key_id (SHA-256 hex of the
	// SPKI) so the operator can correlate with what schedd's
	// NodeKeyRegistry will index against.
	if !strings.Contains(out.String(), "key_id:") {
		t.Errorf("output = %q, want 'key_id:' status line", out.String())
	}
	if !strings.Contains(out.String(), "Wrote") {
		t.Errorf("output = %q, want 'Wrote' status line", out.String())
	}
}

// TestCmdNodeKeyInit_RefusesOverwrite pins the safety rail:
// re-running init against an existing keypair without --force
// returns 1 and the existing priv key is byte-identical after
// the rejected call. The pub file is the second refuse check
// (writeNodeKeyFiles checks both paths).
func TestCmdNodeKeyInit_RefusesOverwrite(t *testing.T) {
	_, readStderr, restore := swapIO(t)
	defer restore()

	dir := t.TempDir()
	privPath := filepath.Join(dir, "node.key")
	pubPath := filepath.Join(dir, "node.pub")

	// First init: succeeds.
	if code := cmdNodeKeyInit([]string{"--node-key", privPath, "--node-key-pub", pubPath}); code != 0 {
		t.Fatalf("first init: %d, want 0", code)
	}
	privBefore, err := os.ReadFile(privPath)
	if err != nil {
		t.Fatalf("read priv before: %v", err)
	}

	// Second init without --force: refuses.
	if code := cmdNodeKeyInit([]string{"--node-key", privPath, "--node-key-pub", pubPath}); code != 1 {
		t.Errorf("second init without force: code = %d, want 1", code)
	}
	if !strings.Contains(readStderr(), "refusing to overwrite") {
		t.Errorf("stderr = %q, want 'refusing to overwrite'", readStderr())
	}
	privAfter, err := os.ReadFile(privPath)
	if err != nil {
		t.Fatalf("read priv after: %v", err)
	}
	if string(privBefore) != string(privAfter) {
		t.Errorf("priv file body changed after refused init")
	}
}

// TestCmdNodeKeyRotate_DefaultForce pins the asymmetric defaults:
// init defaults --force=false (refuse overwrite), rotate defaults
// --force=true (a bare rotate MUST overwrite — rotate without
// overwrite is a no-op, which is meaningless). The asymmetry
// mirrors sign-keys and is load-bearing.
func TestCmdNodeKeyRotate_DefaultForce(t *testing.T) {
	out, restore := swapStdout(t)
	defer restore()

	dir := t.TempDir()
	privPath := filepath.Join(dir, "node.key")
	pubPath := filepath.Join(dir, "node.pub")

	// Seed an existing keypair.
	if code := cmdNodeKeyInit([]string{"--node-key", privPath, "--node-key-pub", pubPath}); code != 0 {
		t.Fatalf("init seed: %d, want 0", code)
	}
	privBefore, err := os.ReadFile(privPath)
	if err != nil {
		t.Fatalf("read priv before: %v", err)
	}

	// Bare rotate — no flags. Default force=true so this MUST
	// overwrite.
	if code := cmdNodeKeyRotate([]string{"--node-key", privPath, "--node-key-pub", pubPath}); code != 0 {
		t.Fatalf("bare rotate: %d, want 0", code)
	}
	privAfter, err := os.ReadFile(privPath)
	if err != nil {
		t.Fatalf("read priv after: %v", err)
	}
	if string(privBefore) == string(privAfter) {
		t.Errorf("priv file body unchanged after rotate (force=true must overwrite)")
	}
	if !strings.Contains(out.String(), "Rotated") {
		t.Errorf("output = %q, want 'Rotated' status line", out.String())
	}
}

// TestCmdNodeKeyStatus_ReportsBothFiles pins the status shape:
// one line per file with mode + sha256[:12] + path; missing files
// print an explicit "missing" line so the operator can see both
// paths even when one is absent (the runbook §3 dry-run depends
// on this).
func TestCmdNodeKeyStatus_ReportsBothFiles(t *testing.T) {
	out, restore := swapStdout(t)
	defer restore()

	dir := t.TempDir()
	privPath := filepath.Join(dir, "node.key")
	// The test deliberately leaves the .pub path absent so the
	// "missing" marker is exercised; passing missingPath as the
	// --node-key-pub flag is the way the operator hits that branch
	// in production (file deleted by hand, or first run after a
	// partial rotate).
	missingPath := filepath.Join(dir, "absent.pub")

	// Seed one file, leave the other absent.
	if err := os.WriteFile(privPath, []byte("placeholder\n"), 0o400); err != nil {
		t.Fatalf("seed priv: %v", err)
	}

	if code := cmdNodeKeyStatus([]string{"--node-key", privPath, "--node-key-pub", missingPath}); code != 0 {
		t.Errorf("cmdNodeKeyStatus = %d, want 0", code)
	}
	got := out.String()
	if !strings.Contains(got, "node.key") {
		t.Errorf("output missing 'node.key' label: %q", got)
	}
	// %#o for FileMode.Perm() renders as `0400` (without 0o prefix
	// because Go's FileMode is an integer type — %#o uses the
	// alternate form but the leading 0o gets suppressed on integer
	// types in some Go versions; pin what the code actually emits).
	if !strings.Contains(got, "0400") {
		t.Errorf("output missing mode 0400: %q", got)
	}
	// sha256[:12] is 6 bytes -> 12 hex chars
	wantSum := sha256.Sum256([]byte("placeholder\n"))
	wantHex := hex.EncodeToString(wantSum[:6])
	if !strings.Contains(got, "sha256:"+wantHex) {
		t.Errorf("output missing sha256:%s: %q", wantHex, got)
	}
	if !strings.Contains(got, "missing") {
		t.Errorf("output missing 'missing' marker for absent file: %q", got)
	}
}

// TestCmdNodeKeyInit_KeyIDMatchesSched pins the wire-shape contract:
// the key_id printed by init must equal sched.KeyIDForPublicKey of
// the just-written priv key. The schedd-side registry is keyed by
// this value; if the CLI computes it differently, schedd rejects
// every report with ErrUnknownNodeKey.
func TestCmdNodeKeyInit_KeyIDMatchesSched(t *testing.T) {
	out, restore := swapStdout(t)
	defer restore()

	dir := t.TempDir()
	privPath := filepath.Join(dir, "node.key")
	pubPath := filepath.Join(dir, "node.pub")

	if code := cmdNodeKeyInit([]string{"--node-key", privPath, "--node-key-pub", pubPath}); code != 0 {
		t.Fatalf("cmdNodeKeyInit = %d, want 0", code)
	}

	keyID, err := reportKeyIDForFile(pubPath)
	if err != nil {
		t.Fatalf("reportKeyIDForFile: %v", err)
	}
	if len(keyID) != 64 {
		t.Errorf("key_id len = %d, want 64 (sha256 hex of SPKI)", len(keyID))
	}
	if !strings.Contains(out.String(), "key_id: "+keyID) {
		t.Errorf("output missing matching key_id %s: %q", keyID, out.String())
	}
}

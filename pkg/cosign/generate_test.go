// Tests for the operator-side keypair writers (WriteKeyPair + WriteKeyPairForGroup).
// These pin the install-topology contract: a non-root daemon (faas-imaged,
// User=faas-imaged Group=faas) must be able to read the signing key via
// group access. The cosign_test.go in this package focuses on the LocalSigner /
// LocalVerifier round-trip — this file is narrowly scoped to the file-write API
// and the umask-defeats-chmod invariant. Per the whitebox-test-file-pattern
// memory, the unexported `writeKeyFile` helper is load-bearing for these tests
// and we want to pin its umask behavior without exposing it.
package cosign

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestWriteKeyPair_OwnerOnlyMode pins the existing 0400/0444 writer behavior.
// A future change to add group-read on the owner-only path trips this test
// before it breaks a single-operator install that depends on 0400.
func TestWriteKeyPair_OwnerOnlyMode(t *testing.T) {
	dir := t.TempDir()
	privPath := filepath.Join(dir, "sign.key")
	pubPath := filepath.Join(dir, "sign-pub.pem")

	privPEM, pubPEM, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	if err := WriteKeyPair(privPath, privPEM, pubPath, pubPEM, false); err != nil {
		t.Fatalf("WriteKeyPair: %v", err)
	}

	privInfo, err := os.Stat(privPath)
	if err != nil {
		t.Fatalf("stat priv: %v", err)
	}
	if got := privInfo.Mode().Perm(); got != 0o400 {
		t.Errorf("priv mode = %#o, want 0o400 (WriteKeyPair owner-only)", got)
	}
	pubInfo, err := os.Stat(pubPath)
	if err != nil {
		t.Fatalf("stat pub: %v", err)
	}
	if got := pubInfo.Mode().Perm(); got != 0o444 {
		t.Errorf("pub mode = %#o, want 0o444", got)
	}

	// Round-trip: the writer output must be acceptable to the loader.
	if _, err := LoadPrivateKeyFile(privPath); err != nil {
		t.Errorf("LoadPrivateKeyFile on 0400 writer output: %v", err)
	}
	if _, err := LoadPublicKeyFile(pubPath); err != nil {
		t.Errorf("LoadPublicKeyFile on writer output: %v", err)
	}
}

// TestWriteKeyPair_RefusesOverwrite asserts the safety rail that prevents a
// botched `faas sign-keys init` invocation from clobbering a live keypair.
// LoadPrivateKeyFile would otherwise silently accept either old or new bytes
// after a partial overwrite — the explicit refusal at write time is the
// defense.
func TestWriteKeyPair_RefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	privPath := filepath.Join(dir, "sign.key")
	pubPath := filepath.Join(dir, "sign-pub.pem")

	privPEM, pubPEM, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	if err := WriteKeyPair(privPath, privPEM, pubPath, pubPEM, false); err != nil {
		t.Fatalf("first write: %v", err)
	}

	// Second call without force must fail on the priv side (which is
	// written first by WriteKeyPair). Capture the bytes on disk before
	// to confirm they weren't partially clobbered.
	privBefore, err := os.ReadFile(privPath)
	if err != nil {
		t.Fatalf("read priv before: %v", err)
	}
	if err := WriteKeyPair(privPath, privPEM, pubPath, pubPEM, false); err == nil {
		t.Fatal("second WriteKeyPair without force: want error, got nil")
	}
	privAfter, err := os.ReadFile(privPath)
	if err != nil {
		t.Fatalf("read priv after: %v", err)
	}
	if string(privBefore) != string(privAfter) {
		t.Error("priv file was mutated despite refusal; the refusal happened after the write completed")
	}
}

// TestWriteKeyPairForGroup_PermsAndUmask is the load-bearing test for the
// DigitalOcean topology. faas-imaged runs as User=faas-imaged Group=faas and
// must be able to read /etc/faas/secrets/sign.key. The bootstrap invokes the
// writer from a script where umask may be 022 (Debian default); the writer
// must chmod AFTER the writeFile call to defeat that umask. Without the
// post-write chmod the file would land at 0444 (writable by owner is masked
// off but group-read survives umask 022 at 0444 — wait, no: os.WriteFile with
// mode 0440 under umask 022 produces 0440 & ^022 = 0440, because 022 removes
// the w bits for group/other which 0440 doesn't have anyway). The actual
// failure mode is umask 007 (group-targeted), under which 0440 written without
// post-chmod would survive but a less-permissive intent could be silently
// widened. This test pins both the umask-defeats-chmod invariant AND the
// group-readable output.
func TestWriteKeyPairForGroup_PermsAndUmask(t *testing.T) {
	dir := t.TempDir()
	privPath := filepath.Join(dir, "sign.key")
	pubPath := filepath.Join(dir, "sign-pub.pem")

	privPEM, pubPEM, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}

	// Set a non-zero umask before the write. syscall.Umask returns the
	// previous value; we restore it at the end of the test.
	const testUmask = 0o077
	oldUmask := syscall.Umask(testUmask)
	t.Cleanup(func() { syscall.Umask(oldUmask) })

	if err := WriteKeyPairForGroup(privPath, privPEM, pubPath, pubPEM, false); err != nil {
		t.Fatalf("WriteKeyPairForGroup: %v", err)
	}

	privInfo, err := os.Stat(privPath)
	if err != nil {
		t.Fatalf("stat priv: %v", err)
	}
	if got := privInfo.Mode().Perm(); got != 0o440 {
		t.Errorf("priv mode = %#o, want 0o440 (group-readable for faas-imaged)", got)
	}
	pubInfo, err := os.Stat(pubPath)
	if err != nil {
		t.Fatalf("stat pub: %v", err)
	}
	if got := pubInfo.Mode().Perm(); got != 0o444 {
		t.Errorf("pub mode = %#o, want 0o444", got)
	}
}

// TestWriteKeyPairForGroup_RefusesOverwrite mirrors the owner-only path: the
// safety rail prevents partial corruption of a live keypair.
func TestWriteKeyPairForGroup_RefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	privPath := filepath.Join(dir, "sign.key")
	pubPath := filepath.Join(dir, "sign-pub.pem")

	privPEM, pubPEM, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	if err := WriteKeyPairForGroup(privPath, privPEM, pubPath, pubPEM, false); err != nil {
		t.Fatalf("first write: %v", err)
	}

	privBefore, err := os.ReadFile(privPath)
	if err != nil {
		t.Fatalf("read priv before: %v", err)
	}
	if err := WriteKeyPairForGroup(privPath, privPEM, pubPath, pubPEM, false); err == nil {
		t.Fatal("second WriteKeyPairForGroup without force: want error, got nil")
	}
	privAfter, err := os.ReadFile(privPath)
	if err != nil {
		t.Fatalf("read priv after: %v", err)
	}
	if string(privBefore) != string(privAfter) {
		t.Error("priv file was mutated despite refusal")
	}
}

// TestWriteKeyPairForGroup_ForceOverwriteRotates pins the rotate path:
// `faas sign-keys rotate --force` is the documented operator flow when the
// existing keypair needs to be replaced (compromise, scheduled rotation).
// After force-rotate, the loader must accept the new bytes — i.e., the new
// keypair must round-trip through GenerateKeyPair + WriteKeyPairForGroup +
// LoadPrivateKeyFile.
func TestWriteKeyPairForGroup_ForceOverwriteRotates(t *testing.T) {
	dir := t.TempDir()
	privPath := filepath.Join(dir, "sign.key")
	pubPath := filepath.Join(dir, "sign-pub.pem")

	// Initial keypair.
	priv1, pub1, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair 1: %v", err)
	}
	if err := WriteKeyPairForGroup(privPath, priv1, pubPath, pub1, false); err != nil {
		t.Fatalf("first write: %v", err)
	}
	loaded1, err := LoadPrivateKeyFile(privPath)
	if err != nil {
		t.Fatalf("LoadPrivateKeyFile after first write: %v", err)
	}

	// Rotated keypair.
	priv2, pub2, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair 2: %v", err)
	}
	if err := WriteKeyPairForGroup(privPath, priv2, pubPath, pub2, true); err != nil {
		t.Fatalf("force rotate: %v", err)
	}

	loaded2, err := LoadPrivateKeyFile(privPath)
	if err != nil {
		t.Fatalf("LoadPrivateKeyFile after rotate: %v", err)
	}

	// Sanity: rotated key is a distinct P-256 key. Compare via the public
	// component — two P-256 keys collide with negligible probability.
	if loaded1.PublicKey.X.Cmp(loaded2.PublicKey.X) == 0 &&
		loaded1.PublicKey.Y.Cmp(loaded2.PublicKey.Y) == 0 {
		t.Error("rotated key has same public coordinates as the original; rotation did not replace bytes")
	}

	// And the file mode survived the rotate.
	privInfo, err := os.Stat(privPath)
	if err != nil {
		t.Fatalf("stat priv after rotate: %v", err)
	}
	if got := privInfo.Mode().Perm(); got != 0o440 {
		t.Errorf("priv mode after rotate = %#o, want 0o440", got)
	}
}

// TestWriteKeyPairForGroup_EmptyPaths pins the empty-path guard. A missing
// path argument in the CLI must surface as an explicit error rather than a
// confusing os.WriteFile errno.
func TestWriteKeyPairForGroup_EmptyPaths(t *testing.T) {
	privPEM, pubPEM, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	if err := WriteKeyPairForGroup("", privPEM, "x", pubPEM, false); err == nil {
		t.Error("WriteKeyPairForGroup with empty privPath: want error, got nil")
	}
	if err := WriteKeyPairForGroup("x", privPEM, "", pubPEM, false); err == nil {
		t.Error("WriteKeyPairForGroup with empty pubPath: want error, got nil")
	}
}

// TestWriteKeyPair_EmptyPaths pins the same guard on the owner-only writer.
// The owner-only path is the alternative install topology — its guard must
// also reject empty paths.
func TestWriteKeyPair_EmptyPaths(t *testing.T) {
	privPEM, pubPEM, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	if err := WriteKeyPair("", privPEM, "x", pubPEM, false); err == nil {
		t.Error("WriteKeyPair with empty privPath: want error, got nil")
	}
	if err := WriteKeyPair("x", privPEM, "", pubPEM, false); err == nil {
		t.Error("WriteKeyPair with empty pubPath: want error, got nil")
	}
}

// tarball_test.go — whitebox tests for pkg/releaseinstall/tarball.go
// (ADR-113 canonical daemon tarball, PR-A commit 1).
//
// External test package, matching the convention
// pkg/releaseinstall/bundle_test.go and pkg/releaseinstall/install_test.go
// follow (one test file per source file, package _test). The
// tests here all use the existing `pkg/releaseinstall.Build` and
// the catalog from `pkg/manifest.SortedHostKeys()` — no new
// dependencies are pulled in.
//
// Verifies:
//   - BuildTarball produces BYTE-IDENTICAL Packed bytes across
//     rebuilds (TestTarball_Build_HashStable).
//   - Tarball.Extract writes the same bytes the manifest advertises
//     (TestTarball_Extract_RoundTrip).
//   - Extract is idempotent: a second extract on top of the first
//     produces the same file bytes (TestTarball_Extract_Idempotent).
//   - Verify fails closed on a tampered Manifest (the cosign wrapper
//     in commit 2 will be the fail-closed cosign path;
//     TestTarball_Verify_RejectsTampered covers the manifest half
//     here, with the SIG path added in commit 2).
package releaseinstall_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/manifest"
	"github.com/onebox-faas/faas/pkg/releaseinstall"
)

// fakeBinDir creates a temporary bin dir containing one file per
// daemon in the canonical catalog. Each file's bytes are
// deterministic (the daemon name repeated until the byte quota is
// met) so two builds against the same bin dir produce identical
// tarballs.
//
// Returned is the bin dir path + a cleanup func the caller defers.
func fakeBinDir(t *testing.T) (binDir, releasesRoot, gitSHA, manifestHash string) {
	t.Helper()
	root := t.TempDir()
	gitSHA = "0123456789abcdef0123456789abcdef01234567"
	manifestHash = "sha256:" + strings.Repeat("a", 64)
	binDir = filepath.Join(root, gitSHA, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	// Use the same catalog as pkg/releaseinstall.Build so a future
	// PR that adds a daemon automatically grows the test.
	for _, name := range manifest.SortedHostKeys() {
		if err := os.WriteFile(filepath.Join(binDir, name), deterministicBody(name), 0o755); err != nil {
			t.Fatalf("write daemon %s: %v", name, err)
		}
	}
	return binDir, root, gitSHA, manifestHash
}

// signedTarball is the helper every happy-path test uses: it
// builds a Tarball and stamps a fake cosign signature onto it so
// Tarball.Verify's cosign half has something to verify against
// the fixture. Commit 2's load-bearing seam: BuildTarball alone
// leaves Sig empty; the fixture verifier is what allows the
// happy-path tests to assert on the identity-stamping side
// without a live cosign binary.
//
// Tests that want to assert the *negative* path (sig missing,
// tampered tarball, etc.) skip this helper and mutate the
// returned Tarball directly.
func signedTarball(t *testing.T) (*releaseinstall.Tarball, string) {
	t.Helper()
	_, root, gitSHA, manifestHash := fakeBinDir(t)
	tb, err := releaseinstall.BuildTarball(root, gitSHA, manifestHash, time.Now().UTC())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	const identity = "https://github.com/poyrazK/faas/.github/workflows/build-sha256.yml@refs/tags/v1.2.3"
	tb.Sig = []byte("fake-cosign-bundle-for-" + identity)
	return tb, identity
}

// catalogDaemons is the in-test alias for the canonical daemon
// catalog. Tests use this to enumerate the set without
// hard-coding names.
func catalogDaemons() []string {
	return manifest.SortedHostKeys()
}

// deterministicBody returns a daemon-name-driven byte slice. The
// byte length is 1024 so sha256 is non-trivial but the test still
// runs in milliseconds.
func deterministicBody(name string) []byte {
	const size = 1024
	out := make([]byte, size)
	for i := range out {
		out[i] = name[i%len(name)]
	}
	return out
}

// TestTarball_Build_HashStable asserts BuildTarball produces the same
// bytes when called twice on the same bin dir (with a different
// `now` value — the load-bearing test that Build doesn't accidentally
// embed a wall-clock value in the tarball).
func TestTarball_Build_HashStable(t *testing.T) {
	_, root, gitSHA, manifestHash := fakeBinDir(t)

	t1, err := releaseinstall.BuildTarball(root, gitSHA, manifestHash, time.Unix(1_700_000_000, 0).UTC())
	if err != nil {
		t.Fatalf("build #1: %v", err)
	}
	t2, err := releaseinstall.BuildTarball(root, gitSHA, manifestHash, time.Unix(1_800_000_000, 0).UTC())
	if err != nil {
		t.Fatalf("build #2: %v", err)
	}

	if len(t1.Packed) != len(t2.Packed) {
		t.Fatalf("packed length differs: %d vs %d (timestamps leaked into the tarball)", len(t1.Packed), len(t2.Packed))
	}
	if !bytes.Equal(t1.Packed, t2.Packed) {
		h1 := sha256.Sum256(t1.Packed)
		h2 := sha256.Sum256(t2.Packed)
		t.Fatalf("packed bytes differ:\n  build #1: %s\n  build #2: %s",
			hex.EncodeToString(h1[:]), hex.EncodeToString(h2[:]))
	}
	// Sanity: the embedded manifest's CreatedAt DOES change between
	// builds (it's part of the manifest, not the tarball bytes),
	// which is why we compare Packed and not the whole struct.
	if t1.Manifest.CreatedAt.Equal(t2.Manifest.CreatedAt) {
		t.Fatalf("manifest CreatedAt must differ between builds — sorted-hosts test is suspect")
	}
}

// TestTarball_Verify_BlessedManifests asserts Verify returns nil
// on a tarball that was just produced and stamped with a fake cosign
// signature (the happy path — no tampering, the fixture verifier
// says "yes, the OIDC identity matches"). Also asserts the cert
// identity got stamped onto Manifest.Signature so audit trails see
// who signed it.
func TestTarball_Verify_BlessedManifests(t *testing.T) {
	tb, wantIdentity := signedTarball(t)
	verifier := &releaseinstall.FixtureCosignVerifier{Identity: wantIdentity}

	gotIdentity, err := tb.Verify(context.Background(), verifier)
	if err != nil {
		t.Fatalf("verify blessed tarball: %v", err)
	}
	if gotIdentity != wantIdentity {
		t.Fatalf("verify blessed tarball: identity = %q, want %q", gotIdentity, wantIdentity)
	}
	if tb.Manifest.Signature != wantIdentity {
		t.Fatalf("verify blessed tarball: manifest.Signature = %q, want %q", tb.Manifest.Signature, wantIdentity)
	}
}

// TestTarball_Verify_RejectsTampered asserts that bit-flipping the
// tarball bytes makes Verify fail closed with ErrTarballTampered.
// The cosign half runs first in the Verify pipeline only AFTER the
// hash-walk half; a tampered Packed will fail hashWalk before the
// fixture verifier is consulted. Commit 2's load-bearing test: even
// if the cosign path were "yes" (the fixture), the hash half refuses
// the tarball — defence-in-depth.
func TestTarball_Verify_RejectsTampered(t *testing.T) {
	tb, identity := signedTarball(t)
	// Flip a bit in the middle of the gz payload.
	tampered := append([]byte(nil), tb.Packed...)
	tampered[len(tampered)/2] ^= 0x01
	tb2 := *tb
	tb2.Packed = tampered

	verifier := &releaseinstall.FixtureCosignVerifier{Identity: identity}
	_, err := tb2.Verify(context.Background(), verifier)
	if err == nil {
		t.Fatalf("verify on tampered tarball: want error, got nil")
	}
	if !errors.Is(err, releaseinstall.ErrTarballTampered) {
		t.Fatalf("verify on tampered tarball: got %v, want ErrTarballTampered", err)
	}
}

// TestTarball_Verify_RejectsMissingSig asserts that a Tarball with
// an empty Sig field fails closed with ErrCosignSigMissing — even
// if the hash walk would otherwise pass. This is the trust-bit
// invariant: a tarball without a sig is NOT a canonical tarball.
func TestTarball_Verify_RejectsMissingSig(t *testing.T) {
	tb, identity := signedTarball(t)
	tb.Sig = nil

	verifier := &releaseinstall.FixtureCosignVerifier{Identity: identity}
	_, err := tb.Verify(context.Background(), verifier)
	if !errors.Is(err, releaseinstall.ErrCosignSigMissing) {
		t.Fatalf("verify on unsigned tarball: got %v, want ErrCosignSigMissing", err)
	}
}

// TestTarball_Verify_NilCosignVerifier asserts the commit-2
// invariant: a nil verifier is a programmer error, surfaced as a
// non-nil error (not a panic on a nil-interface call).
func TestTarball_Verify_NilCosignVerifier(t *testing.T) {
	tb, _ := signedTarball(t)
	_, err := tb.Verify(context.Background(), nil)
	if err == nil {
		t.Fatalf("verify with nil verifier: want error, got nil")
	}
	if errors.Is(err, releaseinstall.ErrTarballTampered) {
		t.Fatalf("verify with nil verifier: should not be ErrTarballTampered, got %v", err)
	}
}

// TestTarball_Verify_PropagatesVerifierErr asserts that when the
// fixture verifier returns an error, Verify wraps and surfaces it
// (not as ErrTarballTampered — the trust-bit path has its own
// error class).
func TestTarball_Verify_PropagatesVerifierErr(t *testing.T) {
	tb, _ := signedTarball(t)
	sentinel := errors.New("fixture: cert expired")
	verifier := &releaseinstall.FixtureCosignVerifier{Err: sentinel}

	_, err := tb.Verify(context.Background(), verifier)
	if !errors.Is(err, sentinel) {
		t.Fatalf("verify with failing verifier: got %v, want wraps %v", err, sentinel)
	}
}

// TestTarball_Extract_RoundTrip builds, extracts, then re-Verifies
// the on-disk tree via the existing pkg/releaseinstall.Verify —
// proving that the tarball round-trips through extraction without
// any byte drift on the catalog binaries.
func TestTarball_Extract_RoundTrip(t *testing.T) {
	bin, root, gitSHA, manifestHash := fakeBinDir(t)
	tb, err := releaseinstall.BuildTarball(root, gitSHA, manifestHash, time.Now().UTC())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// Clear bin/ and re-extract so we know the on-disk bytes
	// come ONLY from the tarball.
	for _, name := range catalogDaemons() {
		if err := os.Remove(filepath.Join(bin, name)); err != nil && !os.IsNotExist(err) {
			t.Fatalf("clear %s: %v", name, err)
		}
	}
	if err := tb.Extract(root); err != nil {
		t.Fatalf("extract: %v", err)
	}
	if err := releaseinstall.Verify(root, tb.Manifest); err != nil {
		t.Fatalf("on-disk verify after extract: %v", err)
	}
}

// TestTarball_Extract_Idempotent extracts twice and asserts the
// on-disk bytes are identical.
func TestTarball_Extract_Idempotent(t *testing.T) {
	bin, root, gitSHA, manifestHash := fakeBinDir(t)
	tb, err := releaseinstall.BuildTarball(root, gitSHA, manifestHash, time.Now().UTC())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := tb.Extract(root); err != nil {
		t.Fatalf("extract #1: %v", err)
	}
	daemons := catalogDaemons()
	hashes1 := make(map[string]string, len(daemons))
	for _, n := range daemons {
		body, err := os.ReadFile(filepath.Join(bin, n))
		if err != nil {
			t.Fatalf("read %s: %v", n, err)
		}
		h := sha256.Sum256(body)
		hashes1[n] = hex.EncodeToString(h[:])
	}

	if err := tb.Extract(root); err != nil {
		t.Fatalf("extract #2: %v", err)
	}
	for _, n := range daemons {
		body, err := os.ReadFile(filepath.Join(bin, n))
		if err != nil {
			t.Fatalf("read %s: %v", n, err)
		}
		h := sha256.Sum256(body)
		got := hex.EncodeToString(h[:])
		if got != hashes1[n] {
			t.Fatalf("daemon %s idempotency: extract #2 sha256=%s, want %s", n, got, hashes1[n])
		}
	}
}

// TestTarball_Extract_RefusesNilTarball defends against the trivial
// misuse path (pointer-deref panics) — Verify / Extract are
// public methods; a nil receiver is the most common mistake callers
// make.
func TestTarball_Extract_RefusesNilTarball(t *testing.T) {
	var tb *releaseinstall.Tarball
	if err := tb.Extract(t.TempDir()); err == nil {
		t.Fatalf("nil tarball Extract: want error, got nil")
	}
}

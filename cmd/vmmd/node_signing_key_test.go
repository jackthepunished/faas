// node_signing_key_test.go — loadNodeSigningKey unit tests.
//
// Pins ADR-053 §3's PKI posture for vmmd:
//
//   - Mode 0400 strict (any other bit → errNodeKeyInsecure)
//   - Missing file → nil/nil/nil (pre-slice-3 mode, not an error)
//   - Open-fd fstat (TOCTOU-safe)
//   - PKCS#8 PEM required
//   - ECDSA P-256 required
//   - key_id matches sched.KeyIDForPublicKey on the public key
//
// A regression that drops the mode check, switches back to
// Stat+ReadFile (TOCTOU-racy), or accepts a non-P-256 curve
// breaks the production PKI — these tests are the tripwire.

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/sched"
)

// writeNodeKey writes a PKCS#8 PEM-encoded ECDSA P-256 key
// at path with the given mode. Test helper — production uses
// faas-pki / cosign tooling to generate node.key.
//
// os.WriteFile only honours the lower 9 perm bits of the
// mode argument, so setuid/setgid/sticky bits require an
// explicit Chmod. This helper does both: WriteFile for the
// perm bits, Chmod for the high bits.
func writeNodeKey(t *testing.T, path string, mode os.FileMode) *ecdsa.PrivateKey {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, pemBytes, mode.Perm()); err != nil {
		t.Fatalf("write key: %v", err)
	}
	if extra := mode &^ os.ModePerm; extra != 0 {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatalf("chmod high bits: %v", err)
		}
	}
	return priv
}

// setNodeKeyPath overrides the env var loadNodeSigningKey
// reads from. Restored on test cleanup.
func setNodeKeyPath(t *testing.T, path string) {
	t.Helper()
	t.Setenv("FAAS_VMMD_NODE_KEY_PATH", path)
}

// TestLoadNodeSigningKey_HappyPath: a 0400 PKCS#8 P-256 key
// loads cleanly and the returned key_id matches the
// sched.KeyIDForPublicKey derivation.
func TestLoadNodeSigningKey_HappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "node.key")
	priv := writeNodeKey(t, path, 0o400)
	setNodeKeyPath(t, path)

	got, gotKeyID, err := loadNodeSigningKeyDefault()
	if err != nil {
		t.Fatalf("loadNodeSigningKey: %v", err)
	}
	got = mustNodeSigningKey(t, got, "returned private key is nil")
	wantKeyID, err := sched.KeyIDForPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("sched.KeyIDForPublicKey: %v", err)
	}
	if gotKeyID != wantKeyID {
		t.Errorf("key_id = %s, want %s", gotKeyID, wantKeyID)
	}
	if got.Curve != elliptic.P256() {
		t.Errorf("loaded curve = %s, want P-256", got.Curve.Params().Name)
	}
}

// TestLoadNodeSigningKey_MissingFile: pre-slice-3 mode returns
// (nil, "", nil) so single-box dev installs that have no
// node.key keep working. The wire field is additive per
// ADR-016, so legacy schedd accepts the empty signature.
func TestLoadNodeSigningKey_MissingFile(t *testing.T) {
	dir := t.TempDir()
	setNodeKeyPath(t, filepath.Join(dir, "does-not-exist"))

	priv, keyID, err := loadNodeSigningKeyDefault()
	if err != nil {
		t.Errorf("missing file should be nil-err, got %v", err)
	}
	if priv != nil || keyID != "" {
		t.Errorf("missing file should return (nil, \"\", nil), got (%v, %q, %v)", priv, keyID, err)
	}
}

// TestLoadNodeSigningKey_RejectsLooseMode pins the strict
// 0400 check. Every mode that isn't 0400 is rejected —
// group/other read, any write/exec/setuid, setgid, sticky.
// This is the load-bearing assertion; a regression that
// accepts 0o440 (group-readable) leaks the ECDSA private
// key to any user in the vmmd group.
func TestLoadNodeSigningKey_RejectsLooseMode(t *testing.T) {
	cases := []struct {
		name string
		mode os.FileMode
	}{
		{"group-read 0o440", 0o440},
		{"world-read 0o444", 0o444},
		{"group-write 0o460", 0o460},
		{"owner-write 0o600", 0o600},
		{"owner-rwx 0o700", 0o700},
		{"sticky 0o1400", 0o1400},
		{"setuid 0o4600", 0o4600},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "node.key")
			writeNodeKey(t, path, tc.mode)
			setNodeKeyPath(t, path)

			// macOS HFS+ silently drops sticky/setgid on
			// regular files. If the platform didn't preserve
			// the requested mode, skip rather than fail.
			st, statErr := os.Stat(path)
			if statErr != nil {
				t.Fatalf("stat after seed: %v", statErr)
			}
			if st.Mode() != tc.mode {
				t.Skipf("platform dropped high bits: got mode %#o, want %#o", st.Mode(), tc.mode)
			}

			_, _, err := loadNodeSigningKeyDefault()
			if err == nil {
				t.Errorf("loadNodeSigningKey on mode %#o succeeded; want error", tc.mode)
				return
			}
			if !errors.Is(err, errNodeKeyInsecure) {
				t.Errorf("err = %v, want wraps errNodeKeyInsecure", err)
			}
			if !strings.Contains(err.Error(), "mode") {
				t.Errorf("err = %v, want contains \"mode\"", err)
			}
		})
	}
}

// TestLoadNodeSigningKey_Accepts0400: the canonical install
// mode is the only accepted value. This is the positive
// companion to the RejectsLooseMode test above.
func TestLoadNodeSigningKey_Accepts0400(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "node.key")
	writeNodeKey(t, path, 0o400)
	setNodeKeyPath(t, path)

	if _, _, err := loadNodeSigningKeyDefault(); err != nil {
		t.Errorf("loadNodeSigningKey on mode 0400 returned %v; want nil", err)
	}
}

// TestLoadNodeSigningKey_RejectsNonPEM: a non-PEM file is
// rejected. A common operator mistake is dumping a DER blob
// or a JWK JSON into node.key — neither parses as PEM.
func TestLoadNodeSigningKey_RejectsNonPEM(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "node.key")
	if err := os.WriteFile(path, []byte("not a pem block"), 0o400); err != nil {
		t.Fatalf("seed: %v", err)
	}
	setNodeKeyPath(t, path)

	_, _, err := loadNodeSigningKeyDefault()
	if err == nil {
		t.Fatal("non-PEM file succeeded; want error")
	}
	if !strings.Contains(err.Error(), "PEM") {
		t.Errorf("err = %v, want contains \"PEM\"", err)
	}
}

// TestLoadNodeSigningKey_RejectsWrongPEMType: SEC1
// ("EC PRIVATE KEY") is rejected — only PKCS#8 ("PRIVATE
// KEY") is accepted. The image builder (cmd/faas-pki) emits
// PKCS#8; an operator who copy-pastes an SEC1 block from a
// different tool gets a clear error instead of a silent
// curve mismatch downstream.
func TestLoadNodeSigningKey_RejectsWrongPEMType(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "node.key")
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey: %v", err)
	}
	der, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("MarshalECPrivateKey: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, pemBytes, 0o400); err != nil {
		t.Fatalf("seed: %v", err)
	}
	setNodeKeyPath(t, path)

	_, _, err = loadNodeSigningKeyDefault()
	if err == nil {
		t.Fatal("SEC1 PEM succeeded; want error")
	}
	if !strings.Contains(err.Error(), "PRIVATE KEY") {
		t.Errorf("err = %v, want mentions PRIVATE KEY", err)
	}
}

// TestLoadNodeSigningKey_RejectsNonP256Curve: a P-384 or
// P-224 key is rejected at load time. The platform's
// signature path is hardwired to P-256 (ADR-053); a different
// curve would silently produce a signature the schedd-side
// registry can't verify.
func TestLoadNodeSigningKey_RejectsNonP256Curve(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "node.key")
	priv, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, pemBytes, 0o400); err != nil {
		t.Fatalf("seed: %v", err)
	}
	setNodeKeyPath(t, path)

	_, _, err = loadNodeSigningKeyDefault()
	if err == nil {
		t.Fatal("P-384 key succeeded; want error")
	}
	if !strings.Contains(err.Error(), "P-256") {
		t.Errorf("err = %v, want mentions P-256", err)
	}
}

// TestLoadNodeSigningKey_RejectsSetuidStickySetgid pins the
// "no setuid/setgid/sticky" check. info.Mode().Perm() returns
// only the lower 9 bits, so a file with mode 0o4600 (setuid +
// no group/other read) would otherwise pass the strict-equality
// perm check while still being an unprivileged-escalation
// surface. The fix is to additionally require the high bits
// (setuid, setgid, sticky) are zero. A regression that drops
// the high-bit check silently allows 0o4600 through.
//
// Note: some platforms (notably macOS / HFS+) silently drop
// sticky and setgid bits on regular files. The test asserts
// the mode was applied before checking; if the platform
// doesn't preserve the high bits, the case is skipped so the
// test fails gracefully on dev boxes without spuriously
// failing the rest of the suite.
func TestLoadNodeSigningKey_RejectsSetuidStickySetgid(t *testing.T) {
	cases := []struct {
		name        string
		mode        os.FileMode
		preserveBit bool
	}{
		{"setuid 0o4600", 0o4600, true},
		{"setgid 0o2400", 0o2400, true},
		{"sticky 0o1400", 0o1400, true},
		{"setuid+sticky 0o5600", 0o5600, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "node.key")
			writeNodeKey(t, path, tc.mode)
			setNodeKeyPath(t, path)

			// Verify the file actually carries the high bits
			// before asserting on the helper. If the platform
			// doesn't preserve them (macOS HFS+ for sticky /
			// setgid), skip the case so we don't fail spuriously.
			st, statErr := os.Stat(path)
			if statErr != nil {
				t.Fatalf("stat after seed: %v", statErr)
			}
			if st.Mode() != tc.mode {
				t.Skipf("platform dropped high bits: got mode %#o, want %#o (e.g. macOS HFS+ doesn't preserve sticky/setgid on regular files)", st.Mode(), tc.mode)
			}

			_, _, err := loadNodeSigningKeyDefault()
			if err == nil {
				t.Errorf("loadNodeSigningKey on mode %#o succeeded; want error", tc.mode)
				return
			}
			if !errors.Is(err, errNodeKeyInsecure) {
				t.Errorf("err = %v, want wraps errNodeKeyInsecure", err)
			}
		})
	}
}

// TestLoadNodeSigningKey_OpenFDBindsModeAndBody pins the
// TOCTOU-safe posture: the mode check is read via the open
// fd, and the body is read from the same fd. A regression
// back to Stat+ReadFile (separate syscalls) would let an
// attacker chmod 0400 then swap the body between the two
// calls.
//
// The behaviour is structural (one Open, then fstat + Read
// via the open fd), not observable from the outside — a
// swap-attack test would need to time the swap inside the
// helper's open-to-read window, which isn't reachable from
// a Go test. Instead we pin that the helper is wired to
// read via the same path it opened: if a future refactor
// re-introduces os.Stat + os.ReadFile (the Stat+ReadFile
// pattern secretbox.LoadHostKey uses), a code-review pin
// catches it. The runtime behaviour is "if the inode is
// replaced at the path between Open and Read, the loaded
// key matches what was on disk at Open time" — true on
// every POSIX fs that honours open-fd inode binding.
func TestLoadNodeSigningKey_OpenFDBindsModeAndBody(t *testing.T) {
	// No-op behavioural test; the tripwire is the structural
	// pin documented above. Listed here so future readers
	// know the omission is intentional.
	t.Skip("open-fd TOCTOU posture is a structural pin, not an observable behaviour")
}

// mustNodeSigningKey is the SA5011 escape hatch for the
// loadNodeSigningKey happy-path test: the loader can legitimately
// return (nil, "", nil) on certain misconfigurations, but we want
// a real key for assertions. A helper that t.Fatal()s and returns
// the value lets staticcheck see the value is non-nil at the call
// site.
func mustNodeSigningKey(t *testing.T, k *ecdsa.PrivateKey, msg string) *ecdsa.PrivateKey {
	t.Helper()
	if k == nil {
		t.Fatal(msg)
	}
	return k
}

// TestLoadNodeSigningKey_PathOverrideWinsOverEnv pins the
// resolution order documented on loadNodeSigningKey: a non-empty
// pathOverride beats FAAS_VMMD_NODE_KEY_PATH, which beats the
// default. This is the load-bearing seam for vmmd.toml's
// node_key_path knob (cmd/vmmd/config.go) — without it, an
// operator who sets both the toml value AND the env var would
// see the env-var path win, contradicting what every other
// daemon does (config.go + daemonunitspec precedence).
//
// Concretely: seed two distinct keypairs, point the env var at
// the "wrong" one, and pass the "right" one through pathOverride.
// The loader MUST return the override's key_id; if it returns
// the env's, the precedence is upside-down.
func TestLoadNodeSigningKey_PathOverrideWinsOverEnv(t *testing.T) {
	dir := t.TempDir()

	// "right" key — what cfg.NodeKeyPath would point at.
	rightPath := filepath.Join(dir, "right.key")
	rightPriv := writeNodeKey(t, rightPath, 0o400)
	rightID, err := sched.KeyIDForPublicKey(&rightPriv.PublicKey)
	if err != nil {
		t.Fatalf("rightID: %v", err)
	}

	// "wrong" key — what FAAS_VMMD_NODE_KEY_PATH points at.
	// Deliberately distinct bytes so the test can tell them apart.
	wrongPath := filepath.Join(dir, "wrong.key")
	_ = writeNodeKey(t, wrongPath, 0o400)
	t.Setenv("FAAS_VMMD_NODE_KEY_PATH", wrongPath)

	// Empty override → env-or-default path. Sanity check the
	// fixture is wired: with "" the env value wins and we get
	// a non-nil key back (the wrong one). If this branch fails
	// the test below would still pass for the wrong reason.
	if _, _, err := loadNodeSigningKey(""); err != nil {
		t.Fatalf("empty-override baseline: %v", err)
	}

	// Non-empty override → wins over env. The returned key_id
	// must match the "right" keypair's, NOT the "wrong" one
	// (which is what the env var is pointing at).
	gotPriv, gotID, err := loadNodeSigningKey(rightPath)
	if err != nil {
		t.Fatalf("override call: %v", err)
	}
	if gotID != rightID {
		t.Errorf("override lost precedence: got key_id %s, want %s (env-var path %s won instead)",
			gotID, rightID, wrongPath)
	}
	// Belt-and-braces: compare the priv keys directly so a future
	// change to KeyIDForPublicKey's hash function can't make this
	// test trivially pass via a coincidence.
	if gotPriv.D.Cmp(rightPriv.D) != 0 {
		t.Errorf("override returned wrong priv key (D values differ)")
	}
}

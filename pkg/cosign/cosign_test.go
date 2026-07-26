// ADR-038 / Tier 3 phase 3 — LocalSigner + LocalVerifier round-trip
// + tamper detection. Pin the wire shape (64-byte raw r||s sig over
// SHA-256(ext4)) so a future change to the cosign package doesn't
// silently invalidate signed layers in storage.
package cosign

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/storage"
)

// memStorage is a StorageBackend backed by a map[string][]byte —
// enough for the unit tests in this package without touching disk.
type memStorage struct {
	blobs map[string][]byte
}

func newMemStorage() *memStorage {
	return &memStorage{blobs: map[string][]byte{}}
}

func (m *memStorage) Put(_ context.Context, key string, r io.Reader) error {
	if key == "" {
		return fmt.Errorf("memstorage: %w: empty", storage.ErrInvalidKey)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.blobs[key] = data
	return nil
}

func (m *memStorage) Get(_ context.Context, key string) (io.ReadCloser, error) {
	if key == "" {
		return nil, fmt.Errorf("memstorage: %w: empty", storage.ErrInvalidKey)
	}
	data, ok := m.blobs[key]
	if !ok {
		return nil, fmt.Errorf("memstorage %q: %w", key, storage.ErrNotFound)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (m *memStorage) Delete(_ context.Context, key string) error {
	delete(m.blobs, key)
	return nil
}

// keyPairTempDir writes a fresh ECDSA P-256 keypair to disk under
// t.TempDir() with the canonical 0400/0444 modes and returns the
// paths. Cleanup is automatic via t.TempDir(). The pub return is
// unused by the current tests but kept for negative-check
// extensions (forged sig with a specific wrong key, etc.).
func keyPairTempDir(t *testing.T) (privPath, pubPath string, pub *ecdsa.PublicKey) {
	t.Helper()
	privPEM, pubPEM, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	dir := t.TempDir()
	privPath = filepath.Join(dir, "sign.key")
	pubPath = filepath.Join(dir, "sign-pub.pem")
	if err := os.WriteFile(privPath, privPEM, 0o400); err != nil {
		t.Fatalf("write priv: %v", err)
	}
	if err := os.WriteFile(pubPath, pubPEM, 0o444); err != nil {
		t.Fatalf("write pub: %v", err)
	}
	pub, err = LoadPublicKeyFile(pubPath)
	if err != nil {
		t.Fatalf("LoadPublicKeyFile: %v", err)
	}
	return privPath, pubPath, pub
}

func TestLocalSigner_RoundTrip(t *testing.T) {
	ctx := context.Background()
	privPath, _, _ := keyPairTempDir(t)
	stor := newMemStorage()

	// Pin the artifact shape: 256 random bytes — small enough to
	// read into memory, large enough to exercise the streaming
	// hash path. Real ext4s are 1-2 GB; the verifier hashes
	// via io.Copy so the size is irrelevant.
	artifact := make([]byte, 256)
	if _, err := rand.Read(artifact); err != nil {
		t.Fatalf("rand: %v", err)
	}
	const layerKey = "apps/test/00000000-0000-0000-0000-000000000000.ext4"
	const sigKey = "sigs/apps/test/00000000-0000-0000-0000-000000000000.ext4.sig"
	if err := stor.Put(ctx, layerKey, bytes.NewReader(artifact)); err != nil {
		t.Fatalf("seed layer: %v", err)
	}

	signer, err := NewLocalSigner(privPath, stor)
	if err != nil {
		t.Fatalf("NewLocalSigner: %v", err)
	}
	if err := signer.Sign(ctx, layerKey, sigKey); err != nil {
		t.Fatalf("Sign: %v", err)
	}

	// Wire-shape pin: the sig blob MUST be exactly 64 bytes (P-256
	// r||s, both 32 bytes). If a future change widens the curve or
	// adds DER headers, this test catches it before the on-disk
	// format changes.
	sig, ok := stor.blobs[sigKey]
	if !ok {
		t.Fatalf("sig key %q not written", sigKey)
	}
	if len(sig) != 64 {
		t.Errorf("sig length = %d, want 64 (P-256 r||s)", len(sig))
	}

	// Verify with a fresh verifier (simulates cmd/schedd
	// startup). The verifier reads from the SAME temp dir as
	// the signer so the keys match.
	pubPath := filepath.Join(filepath.Dir(privPath), "sign-pub.pem")
	verifier, err := NewLocalVerifier(pubPath, stor)
	if err != nil {
		t.Fatalf("NewLocalVerifier: %v", err)
	}
	if err := verifier.Verify(ctx, layerKey, sigKey); err != nil {
		t.Errorf("Verify round-trip failed: %v", err)
	}
}

func TestLocalVerifier_DetectsTamper(t *testing.T) {
	ctx := context.Background()
	privPath, _, _ := keyPairTempDir(t)
	stor := newMemStorage()

	artifact := []byte("hello world")
	const layerKey = "apps/test/tamper.ext4"
	const sigKey = "sigs/apps/test/tamper.ext4.sig"
	if err := stor.Put(ctx, layerKey, bytes.NewReader(artifact)); err != nil {
		t.Fatalf("seed layer: %v", err)
	}

	signer, err := NewLocalSigner(privPath, stor)
	if err != nil {
		t.Fatalf("NewLocalSigner: %v", err)
	}
	if err := signer.Sign(ctx, layerKey, sigKey); err != nil {
		t.Fatalf("Sign: %v", err)
	}

	// Tamper: overwrite the layer with different bytes after the
	// sig was written. The verifier should reject.
	tampered := []byte("goodbye world")
	stor.blobs[layerKey] = tampered

	verifier, err := NewLocalVerifier(filepath.Join(filepath.Dir(privPath), "sign-pub.pem"), stor)
	if err != nil {
		t.Fatalf("NewLocalVerifier: %v", err)
	}
	err = verifier.Verify(ctx, layerKey, sigKey)
	if err == nil {
		t.Fatal("Verify accepted tampered layer; want error")
	}
	// Operator-facing message names the layer key + sig key
	// verbatim (ADR-038 §Consequences Compatibility). Pin the
	// sig_invalid code AND the human-readable fragment.
	if !strings.Contains(err.Error(), "sig_invalid") {
		t.Errorf("err = %v, want code sig_invalid", err)
	}
	if !strings.Contains(err.Error(), layerKey) {
		t.Errorf("err = %v, want layer key in detail", err)
	}
	if !strings.Contains(err.Error(), "signature does not match ext4") {
		t.Errorf("err = %v, want verbatim operator message", err)
	}
}

func TestLocalVerifier_DetectsWrongKey(t *testing.T) {
	// Sign with keypair A; verify with keypair B. This is the
	// "attacker substituted the pub on disk" failure mode — the
	// verifier must reject even though the sig was made by a
	// legitimate P-256 key, because the bytes were signed under a
	// different one.
	ctx := context.Background()
	privPathA, _, _ := keyPairTempDir(t)
	_, pubPathB, _ := keyPairTempDir(t)
	stor := newMemStorage()

	artifact := []byte("payload")
	const layerKey = "apps/test/wrongkey.ext4"
	const sigKey = "sigs/apps/test/wrongkey.ext4.sig"
	if err := stor.Put(ctx, layerKey, bytes.NewReader(artifact)); err != nil {
		t.Fatalf("seed layer: %v", err)
	}

	signer, err := NewLocalSigner(privPathA, stor)
	if err != nil {
		t.Fatalf("NewLocalSigner: %v", err)
	}
	if err := signer.Sign(ctx, layerKey, sigKey); err != nil {
		t.Fatalf("Sign: %v", err)
	}

	// Verifier wired with keypair B's pub.
	verifier, err := NewLocalVerifier(pubPathB, stor)
	if err != nil {
		t.Fatalf("NewLocalVerifier: %v", err)
	}
	if err := verifier.Verify(ctx, layerKey, sigKey); err == nil {
		t.Fatal("Verify accepted sig from wrong key; want error")
	}
}

func TestLocalVerifier_MissingSig(t *testing.T) {
	ctx := context.Background()
	_, pubPath, _ := keyPairTempDir(t)
	stor := newMemStorage()

	artifact := []byte("payload")
	const layerKey = "apps/test/nosig.ext4"
	const sigKey = "sigs/apps/test/nosig.ext4.sig"
	if err := stor.Put(ctx, layerKey, bytes.NewReader(artifact)); err != nil {
		t.Fatalf("seed layer: %v", err)
	}

	verifier, err := NewLocalVerifier(pubPath, stor)
	if err != nil {
		t.Fatalf("NewLocalVerifier: %v", err)
	}
	err = verifier.Verify(ctx, layerKey, sigKey)
	if err == nil {
		t.Fatal("Verify accepted missing sig; want error")
	}
	// "signature missing" is the operator-facing message for
	// this branch (distinct from the "does not match" branch
	// above so the operator can tell storage-rot from
	// tamper-detection at a glance).
	if !strings.Contains(err.Error(), "signature missing") {
		t.Errorf("err = %v, want 'signature missing' in detail", err)
	}
	if !strings.Contains(err.Error(), "sig_invalid") {
		t.Errorf("err = %v, want code sig_invalid", err)
	}
}

func TestLoadPrivateKeyFile_RejectsInsecurePerms(t *testing.T) {
	dir := t.TempDir()
	privPath := filepath.Join(dir, "sign.key")
	privPEM, _, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	// Write with mode 0644 — group-readable is the canonical
	// "insecure perms" signal that LoadPrivateKeyFile must
	// reject. The /etc/faas/secrets/ install uses 0400.
	if err := os.WriteFile(privPath, privPEM, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err = LoadPrivateKeyFile(privPath)
	if !errors.Is(err, ErrInsecurePrivKeyPerms) {
		t.Errorf("err = %v, want ErrInsecurePrivKeyPerms", err)
	}
}

func TestLoadPublicKeyFile_RejectsWritablePub(t *testing.T) {
	dir := t.TempDir()
	pubPath := filepath.Join(dir, "sign-pub.pem")
	_, pubPEM, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	// Mode 0644 is group-writable — the canonical "pub file
	// tampered" signal. The /etc/faas/secrets/ install uses 0444.
	if err := os.WriteFile(pubPath, pubPEM, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err = LoadPublicKeyFile(pubPath)
	if !errors.Is(err, ErrInsecurePubKeyPerms) {
		t.Errorf("err = %v, want ErrInsecurePubKeyPerms", err)
	}
}

// TestSignRawWireShape pins the wire-level contract: the
// concatenation order is r-then-s, each 32 bytes, total 64. If
// a future change reverses the order or widens to a different
// curve, this test breaks before any on-disk data does.
func TestSignRawWireShape(t *testing.T) {
	priv, err := ecdsa.GenerateKey(ecdsaP256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey: %v", err)
	}
	payload := []byte("wire-shape pin")
	sig, err := signRaw(priv, payload)
	if err != nil {
		t.Fatalf("signRaw: %v", err)
	}
	if len(sig) != 64 {
		t.Fatalf("len = %d, want 64", len(sig))
	}
	// Verify round-trip — the verifyRaw side must consume the
	// exact same concatenation.
	digest := sha256.Sum256(payload)
	r, s := newBigInt(sig[:32]), newBigInt(sig[32:])
	if !ecdsa.Verify(&priv.PublicKey, digest[:], r, s) {
		t.Fatal("verifyRoundTrip: sign/verify shape mismatch")
	}
}

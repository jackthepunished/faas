package cosign

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
)

// ErrInsecurePrivKeyPerms is returned by LoadPrivateKeyFile when
// the sign.key file's mode permits any read/write/exec bit to
// group or other. Mirrors secretbox.ErrRecipientInsecurePerms —
// the daemon refuses to start rather than serve a key whose file
// mode has been loosened by a tampered install.
var ErrInsecurePrivKeyPerms = errors.New("cosign: sign.key mode permits group/other access")

// ErrInsecurePubKeyPerms is the corresponding public-side check.
// The public key is by design world-readable, but a writable or
// setuid public-key file is the canonical signal that the host's
// PKI has been tampered with: an attacker who can write to
// sign-pub.pem could substitute their own verifier and have
// schedd accept forged signatures.
var ErrInsecurePubKeyPerms = errors.New("cosign: sign-pub.pem mode permits group/other write/exec/setuid")

// DefaultSignKeyPath is the canonical location for the platform's
// ECDSA P-256 signing key (mode 0400, root:root). The ansible role
// asserts the file exists before systemctl start; cmd/imaged loads
// it at startup with LoadPrivateKeyFile (fail-loud).
const DefaultSignKeyPath = "/etc/faas/secrets/sign.key"

// DefaultSignPubPath is the canonical location for the platform's
// ECDSA P-256 public key (mode 0444). cmd/schedd loads it at
// startup with LoadPublicKeyFile (fail-loud); imaged does NOT
// need the public key.
const DefaultSignPubPath = "/etc/faas/secrets/sign-pub.pem"

// LoadPrivateKeyFile reads + parses + mode-checks the signing
// key. Allowed modes are 0o400 (read-only owner) — no group or
// other access, no exec, no setuid. Returns *ecdsa.PrivateKey on
// success, ErrInsecurePrivKeyPerms on a bad mode, or a parse
// error.
func LoadPrivateKeyFile(path string) (*ecdsa.PrivateKey, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("cosign: stat %q: %w", path, err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("cosign: %q mode %#o: %w",
			path, info.Mode().Perm(), ErrInsecurePrivKeyPerms)
	}
	return loadPrivateKey(path)
}

// LoadPublicKeyFile reads + parses + mode-checks the verification
// key. Allowed modes are 0o400 / 0o440 / 0o404 / 0o444 (owner
// read; group read optional; never writable, never exec, never
// setuid). The public key is by design world-readable, but a
// writable or setuid public-key file is the canonical signal that
// the host's PKI has been tampered with (substitute a hostile
// verifier). Returns *ecdsa.PublicKey on success.
func LoadPublicKeyFile(path string) (*ecdsa.PublicKey, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("cosign: stat %q: %w", path, err)
	}
	const allowedPerm = os.FileMode(0o444)
	if info.Mode().Perm() & ^allowedPerm != 0 {
		return nil, fmt.Errorf("cosign: %q mode %#o: %w",
			path, info.Mode().Perm(), ErrInsecurePubKeyPerms)
	}
	return loadPublicKey(path)
}

// loadPublicKey parses a PEM-encoded SubjectPublicKeyInfo (SPKI)
// ECDSA public key. The key is required to be on the P-256 curve.
func loadPublicKey(path string) (*ecdsa.PublicKey, error) {
	if path == "" {
		return nil, errors.New("cosign: empty public key path")
	}
	raw, err := readFile(path)
	if err != nil {
		return nil, fmt.Errorf("cosign: read public key %q: %w", path, err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("cosign: %q is not PEM-encoded", path)
	}
	if block.Type != "PUBLIC KEY" {
		return nil, fmt.Errorf("cosign: %q: want PEM type PUBLIC KEY, got %q", path, block.Type)
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("cosign: parse SPKI %q: %w", path, err)
	}
	ep, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("cosign: %q is not ECDSA (got %T)", path, pub)
	}
	if ep.Curve != ecdsaP256() {
		return nil, fmt.Errorf("cosign: %q: want P-256 curve, got %s", path, ep.Curve.Params().Name)
	}
	return ep, nil
}

// ecdsaP256 is a single allocation of the P-256 curve (so we can
// compare *elliptic.CurveParams pointers). Cryptographic operations
// stay on the same curve instance.
var p256Curve = elliptic.P256()

func ecdsaP256() elliptic.Curve {
	return p256Curve
}

// newBigInt wraps big.NewInt for the sig-r/s concatenation site.
// P-256 r/s fit in 32 bytes — left-padded by the signer / verifier.
func newBigInt(b []byte) *big.Int {
	return new(big.Int).SetBytes(b)
}

// readFile is a thin wrapper used by the key loaders so the error
// message includes the path. The keys live at /etc/faas/secrets/
// and are read once at daemon startup; performance is irrelevant.
func readFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(f)
}

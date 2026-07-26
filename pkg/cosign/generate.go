package cosign

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
)

// GenerateKeyPair produces a fresh ECDSA P-256 keypair, returning
// the PEM-encoded PKCS#8 private key + SPKI public key bytes.
// Used by the operator-facing `faas keys init` subcommand
// (cmd/faas/keys.go). The function does NOT write to disk; the
// caller (cmd/faas/keys.go) does that with the canonical mode
// (0400 / 0444) and the canonical paths (/etc/faas/secrets/).
func GenerateKeyPair() (privPEM, pubPEM []byte, err error) {
	priv, err := ecdsa.GenerateKey(ecdsaP256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("cosign: generate key: %w", err)
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, fmt.Errorf("cosign: marshal PKCS8: %w", err)
	}
	privPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: privDER,
	})

	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return nil, nil, fmt.Errorf("cosign: marshal SPKI: %w", err)
	}
	pubPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: pubDER,
	})
	return privPEM, pubPEM, nil
}

// WriteKeyPair writes privPEM to privPath (mode 0400) and pubPEM
// to pubPath (mode 0444). Refuses to overwrite existing files
// unless force is true. The operator's `faas keys rotate` path
// sets force=true after archiving the old public key.
func WriteKeyPair(privPath string, privPEM []byte, pubPath string, pubPEM []byte, force bool) error {
	if privPath == "" || pubPath == "" {
		return errors.New("cosign: WriteKeyPair: empty path")
	}
	if err := writeKeyFile(privPath, privPEM, 0o400, force); err != nil {
		return err
	}
	if err := writeKeyFile(pubPath, pubPEM, 0o444, force); err != nil {
		return err
	}
	return nil
}

func writeKeyFile(path string, data []byte, mode os.FileMode, force bool) error {
	if !force {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("cosign: %q already exists (use force=true to overwrite)", path)
		}
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		return fmt.Errorf("cosign: write %q: %w", path, err)
	}
	// Best-effort chmod in case WriteFile was affected by umask.
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("cosign: chmod %q: %w", path, err)
	}
	return nil
}

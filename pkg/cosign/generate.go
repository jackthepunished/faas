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
// Used by the operator-facing `faas sign-keys init` subcommand
// (cmd/faas/commands_sign_keys.go). The function does NOT write
// to disk; the caller (cmdSignKeysInit) writes via
// WriteKeyPairForGroup with the canonical modes (0440 root:faas
// for the private side, 0444 root:root for the public side) and
// the canonical paths (/etc/faas/secrets/). The chown step is
// the installer's responsibility — bootstrap.sh and the ansible
// role set ownership; this package only enforces file mode.
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
// unless force is true. The 0400 form is the owner-only install:
// the file is readable only by the user that owns it, which is
// appropriate when the signing daemon runs as root or as the
// owning user. For the canonical DigitalOcean install where
// faas-imaged runs as User=faas-imaged Group=faas, the install
// must use WriteKeyPairForGroup (0440) instead — a 0400 root-
// owned file is unreadable by the non-root daemon. The operator's
// `faas sign-keys rotate` path sets force=true after archiving
// the old public key. LoadPrivateKeyFile accepts both 0400 and
// 0440, so the verifier side is unaffected.
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

// WriteKeyPairForGroup is the canonical installer writer for the
// DigitalOcean topology: privPEM is written at mode 0440
// (owner+group read) so a non-root faas-imaged process with
// Group=faas can read it via group access. pubPEM is written at
// mode 0444 (world-read), unchanged from WriteKeyPair. Refuses
// to overwrite existing files unless force is true. Ownership
// (root:faas vs root:root) is the installer's responsibility —
// bootstrap.sh and the ansible role chown after this call; the
// cosign package does NOT chown because the install context may
// vary (root in bootstrap, the faas user in ansible, the test
// process in `go test`, and only the install caller knows the
// target user). LoadPrivateKeyFile already accepts 0440, so the
// verifier at schedd startup is unaffected.
func WriteKeyPairForGroup(privPath string, privPEM []byte, pubPath string, pubPEM []byte, force bool) error {
	if privPath == "" || pubPath == "" {
		return errors.New("cosign: WriteKeyPairForGroup: empty path")
	}
	if err := writeKeyFile(privPath, privPEM, 0o440, force); err != nil {
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
	} else {
		// Rotate path: drop the existing file before WriteFile. A
		// 0440/0400 file has no owner-w bit, so an O_WRONLY open on
		// the existing path returns EACCES even though we own the
		// file (POSIX semantics: open mode checked against inode
		// mode, not uid). Removing first lets WriteFile's
		// O_WRONLY|O_CREATE path take over without touching the
		// inode permissions. ENOENT here means the file didn't
		// exist; that's fine — WriteFile will create it.
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("cosign: remove %q for rotate: %w", path, err)
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

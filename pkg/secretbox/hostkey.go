// Package secretbox — sealed-at-rest customer secrets (spec §11/G2).
//
// Threat model summary
//
//   - Plaintext VALUE never enters logs. apid accepts plaintext over TLS,
//     seals it with the host X25519 recipient, and stores the ciphertext.
//   - host.age sits at /etc/faas/secrets/host.age, mode 0400 root:root
//     (spec §11). vmmd reads it (root) when injecting per-wake secrets
//     into the jailer chroot. apid does NOT read it on disk — instead
//     systemd's LoadCredential=faas_host_age_identity (see
//     deploy/systemd/faas-apid.service) copies the file into the apid
//     unit's credential dir owned by faas-apid:faas, so apid's MFA
//     handlers (/verify + /confirm + /recover + /disable, IAM-2 /
//     issue #186) can unseal the TOTP secret without ever opening the
//     0400 root:root file. The on-disk mode and the apid-read path are
//     independent — that decoupling is what lets us hold the 0400
//     contract even though apid needs to consume the identity.
//   - Per-wake injection: vmmd loopback-mounts drive1 and writes
//     /etc/faas/secrets.env (JSON), which guest-init reads after pivot_root.
//     The on-disk format is plain because vmmd is the only root component
//     that touches the jailer chroot; the threat model is the same as
//     /etc/faas/app.json.
//
// What lives here
//
//   - hostkey.go — load/save the host identity to /etc/faas/secrets/host.age;
//     expose the recipient (string) and the identity (for vmmd + apid).
//   - seal.go     — Seal / Open round-trip on an arbitrary env map.
//
// Wire shape: the on-disk envelope is age's standard format (Stanza header +
// ChaCha20-Poly1305 body). The plaintext is a canonical-JSON encoding of
// Envelope (map[string]string) so the same decoder can validate shape on
// Open.
package secretbox

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"filippo.io/age"
)

// ErrRecipientInsecurePerms is returned by LoadRecipient when host.age.pub's
// mode allows write/exec/setuid to anyone other than the file's owner. The
// public half is by design world-readable, but a public-key file that is also
// writable (or setuid) is the canonical signal that the host has been
// tampered with: an attacker who can write to host.age.pub can substitute
// their own recipient and start collecting freshly-sealed ciphertexts.
// Fail-fast at apid startup rather than serving sealed blobs to a
// hijacked recipient.
var ErrRecipientInsecurePerms = errors.New("secretbox: host.age.pub permissions allow write/exec/setuid to non-owner")

// DefaultHostKeyPath is where vmmd looks for (and auto-generates) the host
// X25519 identity on first boot. Spec §11: secrets in /etc/faas/secrets/,
// mode 0400 root:root. apid reads via systemd LoadCredential, not the
// on-disk file — see the package docstring above for the decoupling.
const DefaultHostKeyPath = "/etc/faas/secrets/host.age"

// ErrHostKeyInsecurePerms is returned by LoadHostKey when the file's mode
// allows anything other than owner-read. The private half of the host identity
// is the secret material every SealedSecret in app_secrets is encrypted
// against; a host.age that's group- or world-readable lets any unprivileged
// user on the box extract the X25519 secret key and unseal every customer's
// env vars. Spec §11: secrets in /etc/faas/secrets/ root:root 0400. This
// runtime check is the tripwire when the file's been chmod'd loose (operator
// drift) or restored from a backup with wrong mode.
var ErrHostKeyInsecurePerms = errors.New("secretbox: host.key permissions allow read/write/exec/setuid to non-owner")

// ErrHostKeyNotFound is returned by LoadHostKey when the file is missing.
// Callers (vmmd) treat this as the first-boot signal and call
// GenerateAndSaveHostKey to create one.
var ErrHostKeyNotFound = errors.New("secretbox: host key not found")

// LoadHostKey parses an age-format X25519 identity from path. The file is the
// raw "AGE-SECRET-KEY-1..." string (age's standard textual representation).
//
// Security check (M8 §11): refuse to load if the file's mode permits anything
// other than owner-read (0o400). The private half is the secret material that
// decrypts every customer's sealed env blobs — a single bit set for group,
// other, or any write/exec/setuid is enough for an attacker to extract it.
// Mirrors the public-side check in LoadRecipient, but stricter: 0o400 only.
// ErrHostKeyInsecurePerms is returned for any deviation; the vmmd startup
// path fails-fast on this so a stolen/loosened host.age cannot reach the
// unseal path.
func LoadHostKey(path string) (*age.X25519Identity, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrHostKeyNotFound
		}
		return nil, fmt.Errorf("secretbox: stat host key %q: %w", path, err)
	}
	// The private key must be owner-read ONLY — exact mode 0o400.
	// Any other bit (group/other read, any write/exec/suid) is
	// rejected. The previous check used `Perm() & ^0o400 != 0`,
	// which is equivalent to `Perm() != 0o400` but was easy to
	// misread as "reject anything except owner-read" — and a
	// regression could quietly start accepting 0o440 / 0o444
	// (group/world-readable), which would let any unprivileged
	// user on the box extract the X25519 secret key and unseal
	// every customer's env vars. The exact-equality form is the
	// safest expression of the contract.
	if info.Mode().Perm() != 0o400 {
		return nil, fmt.Errorf("secretbox: host key %q mode %#o: %w",
			path, info.Mode().Perm(), ErrHostKeyInsecurePerms)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("secretbox: read host key %q: %w", path, err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("secretbox: host key %q is empty", path)
	}
	id, err := age.ParseX25519Identity(string(data))
	if err != nil {
		return nil, fmt.Errorf("secretbox: parse host key %q: %w", path, err)
	}
	return id, nil
}

// GenerateAndSaveHostKey creates a new X25519 identity, writes its textual
// representation to path with mode 0400, and returns the identity. Called
// by vmmd on first boot when LoadHostKey returns ErrHostKeyNotFound.
//
// File mode is 0400 root:root — spec §11 literal. The on-disk file is
// only readable by vmmd (root), which is exactly the threat model: a
// single root component owns the secret material. apid reads the
// identity through systemd's LoadCredential=faas_host_age_identity
// (see deploy/systemd/faas-apid.service), which copies the file into
// the unit's credential dir owned by faas-apid:faas. The on-disk
// mode and the credential-dir mode are independent — apid never
// touches /etc/faas/secrets/host.age directly.
//
// On a fresh box this is the bootstrap moment: vmmd is the only root
// component, so it owns key generation. apid never generates — it
// consumes the recipient + identity via LoadCredential.
func GenerateAndSaveHostKey(path string) (*age.X25519Identity, error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, fmt.Errorf("secretbox: generate host key: %w", err)
	}
	if err := os.WriteFile(path, []byte(id.String()), 0o400); err != nil {
		return nil, fmt.Errorf("secretbox: write host key %q: %w", path, err)
	}
	return id, nil
}

// RecipientString returns the age-1... bech32 encoding of id's public half.
// apid stores this in memory and uses it for Seal; vmmd uses the identity
// for Open. The pair (RecipientString, identity) is the only state
// secretbox cares about.
func RecipientString(id *age.X25519Identity) string {
	return id.Recipient().String()
}

// DefaultHostAgeRecipientPath is where vmmd writes the host age recipient
// (public side) for apid to consume. vmmd owns the private half; apid reads
// only the public string at startup. Mode 0444 — public by design.
const DefaultHostAgeRecipientPath = "/etc/faas/secrets/host.age.pub"

// WriteRecipientFile writes the public side of id to path with mode 0444.
// Called by vmmd after LoadOrGenerate. apid reads the file at startup;
// both daemons are owned by root so the file is root-readable (and the
// public key is intentionally non-secret anyway).
func WriteRecipientFile(path string, id *age.X25519Identity) error {
	if err := os.WriteFile(path, []byte(RecipientString(id)), 0o444); err != nil {
		return fmt.Errorf("secretbox: write recipient %q: %w", path, err)
	}
	return nil
}

// LoadRecipient parses a recipient string from path. apid calls this at
// startup to obtain the sealing key. The file is the canonical host.age.pub
// artifact written by vmmd (mode 0444).
//
// Security check: refuse to start if the file's mode permits write/exec/
// setuid to anyone other than the file's owner. The recipient is technically
// public, but a public-key file that is ALSO writable (or setuid) is the
// canonical signal that the host's PKI has been tampered with: an attacker
// who can write to host.age.pub can substitute their own recipient and
// start collecting freshly-sealed ciphertexts. Fail-fast at apid startup
// rather than serving sealed blobs to a hijacked recipient.
//
// Permitted shapes: any combination of read bits for owner (0o400),
// owner-write (0o200), group-read (0o040), and other-read (0o004). That
// accepts the production modes 0o400, 0o440, 0o404, 0o444. Any other bit
// (group/other write, any exec, setuid, setgid, sticky) is rejected.
func LoadRecipient(path string) (*age.X25519Recipient, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("secretbox: stat recipient %q: %w", path, err)
	}
	const allowedPerm = os.FileMode(0o644)
	if info.Mode().Perm() & ^allowedPerm != 0 {
		return nil, fmt.Errorf("secretbox: recipient %q mode %#o: %w",
			path, info.Mode().Perm(), ErrRecipientInsecurePerms)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("secretbox: read recipient %q: %w", path, err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("secretbox: recipient file %q is empty", path)
	}
	r, err := age.ParseX25519Recipient(strings.TrimSpace(string(data)))
	if err != nil {
		return nil, fmt.Errorf("secretbox: parse recipient %q: %w", path, err)
	}
	return r, nil
}

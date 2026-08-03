// Token / secret loader for gatewayd-public (spec §11/G2). Mirrors
// cmd/gatewayd/secrets.go verbatim so the operator-facing error
// messages and the per-file perm check are identical across the two
// daemons. Both daemons read the same Hetzner DNS API token file
// (the renewal race is closed by the certsync leader election, not
// by splitting the load — the file is read once at boot).
//
// Why this lives in cmd/gatewayd-public (not pkg/gateway):
//
//   - pkg/gateway is a shared lib; importing pkg/secretbox from it
//     would force every consumer (test binaries, alternate frontends)
//     to take pkg/secretbox's deps. Keeping the loader in cmd/
//     keeps pkg/gateway free of the 0400 perm-check dependency.
//   - The Hetzner token is a plain bearer token and doesn't share
//     the age X25519 shape in pkg/secretbox.
//
// The shared file path (`/etc/faas/secrets/hetzner-dns.token`,
// matching the legacy cmd/gatewayd behaviour) is the operator
// contract — the ansible role provisions the same file for both
// daemons. The path is overridable via FAAS_HETZNER_DNS_TOKEN_PATH
// at the env layer for the e2e harness + dev-box workflows.
package main

import (
	"errors"
	"fmt"
	"os"
)

// ErrInsecureSecretPerms is returned when a token file is group/other
// writable or has any exec/setuid bits. The error is intentionally distinct
// from "file not found" so an operator can tell "didn't provision" apart from
// "provisioned insecurely".
var ErrInsecureSecretPerms = errors.New("gatewayd-public: secret file mode permits more than owner read/write")

// allowedSecretPerm reports whether perm is a safe mode for a token
// file. The daemon runs as faas:faas per the systemd unit. The only
// safe perms are owner-only OR owner+group-read with the file's
// group set to the daemon's group (`faas`). We accept:
//
//	0400, 0440, 0600, 0640   — owner and optionally group read/write
//
// and reject everything else — other-readable is a leak, group-writable
// is a privilege-escalation signal (any process running as faas could
// overwrite the gateway's ACME credentials), and any exec / setuid /
// setgid / sticky bit is the canonical privilege-escalation signal.
//
// The list is explicit (rather than a bitmask) because a bitmask
// cannot distinguish "group-r allowed, group-w forbidden" — and
// group-w is the canonical priv-esc signal we MUST close on.
func allowedSecretPerm(perm os.FileMode) bool {
	switch perm {
	case 0o400, 0o440, 0o600, 0o640:
		return true
	}
	return false
}

// loadSecretFile reads path and returns its trimmed contents. Fails
// closed if the file doesn't exist, is empty, or has loose
// permissions. Mirrors pkg/secretbox.LoadRecipient so the
// operator-facing error messages have the same shape across the
// platform.
func loadSecretFile(path string) (string, error) {
	if path == "" {
		return "", errors.New("gatewayd-public: secret path is empty")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("gatewayd-public: stat secret %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("gatewayd-public: secret %q is not a regular file (mode %s)", path, info.Mode())
	}
	if !allowedSecretPerm(info.Mode().Perm()) {
		return "", fmt.Errorf("gatewayd-public: secret %q mode %#o: %w",
			path, info.Mode().Perm(), ErrInsecureSecretPerms)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("gatewayd-public: read secret %q: %w", path, err)
	}
	// Trim trailing newline (most operators provision the file with
	// `echo "$TOKEN" > path`). Don't trim interior whitespace — tokens
	// can legitimately contain leading/trailing non-newline whitespace
	// (though they shouldn't).
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	if len(b) == 0 {
		return "", fmt.Errorf("gatewayd-public: secret %q is empty", path)
	}
	return string(b), nil
}

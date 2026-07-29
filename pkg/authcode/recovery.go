// Package authcode holds the recovery-code primitives that are shared
// across pkg/auth (cmd/apid's IAM-2 surface), pkg/state (the
// MFARecoveryCodesHash column tests), and any future daemon that
// needs to mint or verify single-use MFA fallback codes.
//
// Lives outside pkg/auth on purpose: pkg/auth now imports pkg/state
// (the Middleware lift, ADR-044), so pkg/state's *_test.go files
// can no longer import pkg/auth — Go's compiler forbids closing the
// cycle in test compilation. This package has zero onebox imports,
// so both pkg/auth and pkg/state (and their tests) can depend on it
// without creating a cycle.
//
// Error prefixes keep the legacy "auth/totp:" tag (not "authcode:")
// so log greps + errors.Is probes written before the lift (e.g.
// "auth/totp: rand:" in cmd/apid's deploy error budget tracker) keep
// matching. The package itself is new; the error prefix is a log
// compatibility shim.
//
// Entropy breakdown (mirrored from the original pkg/auth/totp.go):
// 10 bytes of CSPRNG source = 80 bits; the base32-no-padding encoding
// of 10 bytes is 16 chars; we truncate to 10 chars (50 bits
// customer-visible — 10 × 5 bits/char over the base32 alphabet
// A-Z + 2-7). 50 bits is enough that brute-forcing a 10-entry hash
// table from a leaked blob is computationally infeasible without
// offline storage of the plaintexts, which never leaves the
// customer's account.
package authcode

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"strings"
)

// RecoveryCodeCount is the number of single-use recovery codes
// minted at enrollment. 10 is the Google Authenticator default and
// the lowest power of 10 that survives one lost phone without a
// customer panic. The metadata comment in the migration notes the
// memory cost: 10 codes * 32 B = 320 B per MFA-enrolled account.
const RecoveryCodeCount = 10

// NewRecoveryCodes returns n plaintext recovery codes (base32, 10
// chars each, no padding) plus the SHA-256 hashes of the same
// plaintexts. The plaintexts are returned to the dashboard ONCE
// (the enroll response); the hashes are what /v1/account/mfa/recover
// compares against.
func NewRecoveryCodes(n int) (plaintexts []string, hashes [][]byte, err error) {
	plaintexts = make([]string, n)
	hashes = make([][]byte, n)
	for i := 0; i < n; i++ {
		raw := make([]byte, 10) // 80 bits = 16 base32 chars; truncate to 10
		if _, err := rand.Read(raw); err != nil {
			return nil, nil, fmt.Errorf("auth/totp: rand: %w", err)
		}
		encoded := strings.TrimRight(base32.StdEncoding.EncodeToString(raw), "=")
		plaintexts[i] = strings.ToUpper(encoded[:10])
		h := sha256.Sum256([]byte(plaintexts[i]))
		hashes[i] = h[:]
	}
	return plaintexts, hashes, nil
}

// HashRecoveryCode is the SHA-256 single-shot helper used by
// /v1/account/mfa/recover to test a presented code against the
// stored hashes. The caller uppercases the presented code before
// passing it in — the recovery-code generator above emits
// uppercase, and case-insensitive matching is the customer's
// expectation (RFC 6238 doesn't mandate case for the TOTP secret,
// but the recovery codes are user-typed and so should accept
// either case).
func HashRecoveryCode(code string) []byte {
	h := sha256.Sum256([]byte(strings.ToUpper(code)))
	return h[:]
}

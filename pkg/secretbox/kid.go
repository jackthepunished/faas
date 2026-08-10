// kid.go — kid (key-id) stamping helpers for ADR-089 PR-A.
//
// The kid is the canonical age-1... recipient string of the host
// identity that sealed a particular ciphertext. ADR-089 PR-A adds
// a `kid text` column to app_secrets (migration 00166) so operators
// can answer "what key sealed this row?" without parsing the
// ciphertext blob.
//
// LoadHostKeys() returns identities in a fixed order: current
// first, previous second (hostkey.go:253). IdentityFingerprint
// reads identity[0] which is the "current" recipient by convention.
// The package-level docstring on seal.go:30 already establishes the
// invariant: "Seal is single-recipient (current only); OpenMulti
// tries every supplied identity until one decrypts." kid stamping
// follows the same single-recipient convention — the kid column
// reflects the SEAL identity (current), not the unseal identity.
//
// Use case matrix:
//
//	┌─────────────────────┬──────────────────┬──────────────────┐
//	│ Re-seal happened?   │ kid column value │ Unseals under    │
//	├─────────────────────┼──────────────────┼──────────────────┤
//	│ never               │ current          │ current          │
//	│ pre-rotation        │ (NULL → backfill)│ previous (still) │
//	│ post-rotation       │ current          │ current          │
//	└─────────────────────┴──────────────────┴──────────────────┘
//
// After rekey completes, every row has kid = current and unseals
// under current only. Pre-rekey rows have kid = previous (or NULL
// if backfilled via the Go-side rekey path) but unseal under
// either via OpenMulti — the kid column is observability, not
// enforcement.
package secretbox

import (
	"errors"

	"filippo.io/age"
)

// IdentityFingerprint returns the age-1... recipient string of the
// CURRENT identity in the supplied slice. "Current" by convention
// is the first identity in the slice (matches LoadHostKeys(dir)
// ordering: current first, previous second).
//
// Returns an error if identities is empty or the first slot is nil.
// This matches the precondition contract of OpenMulti
// (seal.go:115) which also refuses empty / nil-only slices.
//
// ADR-089 PR-A writes IdentityFingerprint(identities) into the
// app_secrets.kid column at every Seal. The rekey.Replayer reads
// the kid column back to decide which rows still need re-sealing
// (kid != current → candidate for re-seal; kid == current → skip).
//
// Stable across identity regeneration: the recipient string is a
// pure function of the public half of an X25519 identity, so
// rebuilding the same identity from its private half yields the
// same recipient string. Operators comparing kid values across
// host migrations should NOT expect a stable mapping — kid is
// scoped to the local box's identity, not a global PKI.
func IdentityFingerprint(identities []*age.X25519Identity) (string, error) {
	if len(identities) == 0 {
		return "", errors.New("secretbox: no identities supplied")
	}
	if identities[0] == nil {
		return "", errors.New("secretbox: current identity is nil")
	}
	return identities[0].Recipient().String(), nil
}

// CurrentRecipient returns the parsed *age.X25519Recipient of the
// CURRENT identity. Use this when you need the recipient object
// itself (e.g. to call Seal) rather than its string fingerprint.
//
// Equivalent to:
//
//	identities[0].Recipient()
//
// but with the same nil / empty precondition checks as
// IdentityFingerprint so callers don't have to duplicate them.
func CurrentRecipient(identities []*age.X25519Identity) (*age.X25519Recipient, error) {
	if len(identities) == 0 {
		return nil, errors.New("secretbox: no identities supplied")
	}
	if identities[0] == nil {
		return nil, errors.New("secretbox: current identity is nil")
	}
	return identities[0].Recipient(), nil
}

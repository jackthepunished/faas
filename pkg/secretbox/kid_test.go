// kid_test.go — unit tests for IdentityFingerprint + CurrentRecipient.
//
// Coverage:
//   - Empty slice → error (precondition contract).
//   - Nil first slot → error (precondition contract).
//   - Single identity → returns its recipient string.
//   - Multiple identities → always reads identities[0] (current),
//     never identities[1] (previous). This is the load-bearing
//     ordering invariant that LoadHostKeys depends on.
//
// We don't test against OpenMulti / Seal here — those are covered
// in seal_test.go and ADR-057. kid.go is a thin stamping helper;
// the only behavioural contract worth pinning is "always read
// identity[0]".
package secretbox

import (
	"errors"
	"strings"
	"testing"

	"filippo.io/age"
)

// TestIdentityFingerprintEmpty: precondition contract — empty
// slice is the canonical "no identities" error.
func TestIdentityFingerprintEmpty(t *testing.T) {
	_, err := IdentityFingerprint(nil)
	if err == nil {
		t.Fatal("expected error for empty identities slice")
	}
	if !strings.Contains(err.Error(), "no identities") {
		t.Fatalf("got %v, want error containing 'no identities'", err)
	}
}

// TestIdentityFingerprintNilFirst: precondition contract — nil
// first slot. Mirrors OpenMulti's nil-filter path (seal.go:122).
func TestIdentityFingerprintNilFirst(t *testing.T) {
	_, err := IdentityFingerprint([]*age.X25519Identity{nil})
	if err == nil {
		t.Fatal("expected error for nil first identity")
	}
	if !strings.Contains(err.Error(), "current identity is nil") {
		t.Fatalf("got %v, want error containing 'current identity is nil'", err)
	}
}

// TestIdentityFingerprintSingle: happy path. Returns the single
// identity's recipient string, which is the canonical age-1...
// bech32 representation (Recipient().String() per filippo.io/age).
func TestIdentityFingerprintSingle(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	got, err := IdentityFingerprint([]*age.X25519Identity{id})
	if err != nil {
		t.Fatalf("IdentityFingerprint: %v", err)
	}
	want := id.Recipient().String()
	if got != want {
		t.Fatalf("IdentityFingerprint = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, "age1") {
		t.Fatalf("IdentityFingerprint = %q, want age-1... prefix", got)
	}
}

// TestIdentityFingerprintOrdering: load-bearing — must read
// identities[0] (current), NOT identities[1] (previous). Two
// freshly-generated identities have different recipients; if the
// implementation accidentally read identities[1] this test would
// fail with a non-empty diff.
func TestIdentityFingerprintOrdering(t *testing.T) {
	current, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate current: %v", err)
	}
	previous, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate previous: %v", err)
	}
	got, err := IdentityFingerprint([]*age.X25519Identity{current, previous})
	if err != nil {
		t.Fatalf("IdentityFingerprint: %v", err)
	}
	if got == previous.Recipient().String() {
		t.Fatal("IdentityFingerprint returned the PREVIOUS identity's recipient; ordering invariant broken")
	}
	if got != current.Recipient().String() {
		t.Fatalf("IdentityFingerprint = %q, want %q (current)",
			got, current.Recipient().String())
	}
}

// TestCurrentRecipient: CurrentRecipient returns the parsed
// *age.X25519Recipient, not just its string. Verify it round-trips
// through RecipientString and that errors mirror IdentityFingerprint.
func TestCurrentRecipient(t *testing.T) {
	// Empty slice.
	if _, err := CurrentRecipient(nil); err == nil {
		t.Fatal("expected error for empty identities slice")
	} else if !strings.Contains(err.Error(), "no identities") {
		t.Fatalf("got %v, want 'no identities' error", err)
	}

	// Nil first slot.
	if _, err := CurrentRecipient([]*age.X25519Identity{nil}); err == nil {
		t.Fatal("expected error for nil first identity")
	} else if !strings.Contains(err.Error(), "current identity is nil") {
		t.Fatalf("got %v, want 'nil' error", err)
	}

	// Happy path: round-trip recipient string.
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	r, err := CurrentRecipient([]*age.X25519Identity{id})
	if err != nil {
		t.Fatalf("CurrentRecipient: %v", err)
	}
	if r.String() != id.Recipient().String() {
		t.Fatalf("CurrentRecipient() round-trip mismatch: %q vs %q",
			r.String(), id.Recipient().String())
	}
}

// TestIdentityFingerprintStable: rebuilding the same identity
// from its textual representation yields the same recipient
// string. This is the property that lets kid columns stay stable
// across daemon restarts (the identity file is reloaded but the
// underlying X25519 key material is identical).
func TestIdentityFingerprintStable(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	// Reload via age.ParseX25519Identity (the same path LoadHostKey
	// uses, kid.go mirror).
	reloaded, err := age.ParseX25519Identity(id.String())
	if err != nil {
		t.Fatalf("reparse identity: %v", err)
	}
	got, err := IdentityFingerprint([]*age.X25519Identity{reloaded})
	if err != nil {
		t.Fatalf("IdentityFingerprint: %v", err)
	}
	if got != id.Recipient().String() {
		t.Fatalf("kid changed after reparse: got %q want %q",
			got, id.Recipient().String())
	}
	// Sanity: reloaded and original should be the same key material.
	if !errors.Is(err, err) { // always false; we just want `err` to be referenced
		_ = err
	}
}

package secretbox

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"
)

// TestSealOpenRoundTrip exercises the happy path: Seal → Open returns the
// same map. Multiple iterations cover ordering / multi-entry cases.
func TestSealOpenRoundTrip(t *testing.T) {
	id := mustGenHostKey(t, "host.age")
	cases := []Envelope{
		{"STRIPE_KEY": "sk_live_abcdef0123456789"},
		{"A": "1", "B": "2", "C": "3"},
		{"WITH_EQUALS": "key=value=with=equals=inside"},
		{"UNICODE_OK_KEY": "value with spaces and punctuation!@#"},
	}
	for _, env := range cases {
		blob, err := Seal(id.Recipient(), env)
		if err != nil {
			t.Fatalf("Seal(%v): %v", env, err)
		}
		got, err := Open(id, blob)
		if err != nil {
			t.Fatalf("Open after Seal(%v): %v", env, err)
		}
		if !envelopesEqual(env, got) {
			t.Errorf("round-trip mismatch: got %v want %v", got, env)
		}
	}
}

// TestOpenTampered asserts that flipping a single byte of the ciphertext
// causes Open to fail. Confirms we're using an AEAD, not just XOR.
func TestOpenTampered(t *testing.T) {
	id := mustGenHostKey(t, "host.age")
	blob, err := Seal(id.Recipient(), Envelope{"KEY": "secret"})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// Flip a byte in the body (skip the ASCII stanza header — first
	// newline + 2 blank lines are typical age format; flip byte 60 which
	// is well inside the binary body).
	idx := len(blob) - 4
	blob[idx] ^= 0xff
	if _, err := Open(id, blob); err == nil {
		t.Fatal("Open succeeded on tampered ciphertext — AEAD broken")
	}
}

// TestOpenWrongIdentity asserts that a different X25519 identity fails to
// decrypt. Confirms the recipient binding is real.
func TestOpenWrongIdentity(t *testing.T) {
	id1 := mustGenHostKey(t, "a.age")
	id2 := mustGenHostKey(t, "b.age")
	blob, _ := Seal(id1.Recipient(), Envelope{"KEY": "secret"})
	if _, err := Open(id2, blob); err == nil {
		t.Fatal("Open with wrong identity succeeded")
	}
}

// TestSealRejectsBadKey ensures the regex check fires before the seal.
// An invalid key must produce an error and NOT call age.
func TestSealRejectsBadKey(t *testing.T) {
	id := mustGenHostKey(t, "host.age")
	for _, bad := range []string{"", "1STARTS_WITH_DIGIT", "lower", "WITH-DASH", "WITH SPACE"} {
		if _, err := Seal(id.Recipient(), Envelope{bad: "v"}); err == nil {
			t.Errorf("Seal(%q): expected error", bad)
		}
	}
}

// TestSealOneEnforcesByteCap asserts the byte cap is checked in the seal
// path (not just at the HTTP layer) so no over-cap ciphertext ever lands in
// PG. SealOne returns the api.Problem-shaped error so callers don't need
// to import pkg/api for the code.
func TestSealOneEnforcesByteCap(t *testing.T) {
	id := mustGenHostKey(t, "host.age")
	blob, err := SealOne(id.Recipient(), "OK_KEY", "short", 10)
	if err != nil {
		t.Fatalf("under-cap: %v", err)
	}
	if len(blob) == 0 {
		t.Fatal("under-cap returned empty blob")
	}
	if _, err := SealOne(id.Recipient(), "OK_KEY", "this string is way too long for the cap", 10); err == nil {
		t.Fatal("over-cap: expected error")
	}
	// maxValueBytes=0 disables the cap (used by bulk Seal path).
	if _, err := SealOne(id.Recipient(), "OK_KEY", "anything goes here", 0); err != nil {
		t.Fatalf("cap disabled: %v", err)
	}
}

// TestSealNilArgs checks the precondition errors. Defensive — callers
// should always pass real args, but a nil recipient or identity must not
// panic.
func TestSealNilArgs(t *testing.T) {
	if _, err := Seal(nil, Envelope{"K": "v"}); err == nil {
		t.Error("Seal(nil recipient): expected error")
	}
	if _, err := Open(nil, []byte("blob")); err == nil {
		t.Error("Open(nil identity): expected error")
	}
	if _, err := Open(&age.X25519Identity{}, []byte{}); err == nil {
		t.Error("Open(empty blob): expected error")
	}
}

// TestSealBinaryOutput is a smoke test that the ciphertext is binary-safe
// (no embedded NULs would surprise PG bytea).
func TestSealBinaryOutput(t *testing.T) {
	id := mustGenHostKey(t, "host.age")
	blob, err := Seal(id.Recipient(), Envelope{"K": "value"})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if len(blob) < 64 {
		t.Errorf("blob suspiciously short: %d bytes", len(blob))
	}
	if !bytes.HasPrefix(blob, []byte("age-encryption.org/v1\n")) {
		t.Errorf("blob missing age header magic")
	}
}

// TestSealBytesOpenBytesRoundTrip exercises the byte-oriented seal
// path (issue #396 / ADR-045 PR 3). The bytes are opaque to apid —
// only the dispatcher (PR 4) reads them back. The namespace tag
// is preserved across the round-trip so a future multi-secret-per-
// blob case can disambiguate.
//
// Plaintext contents test:
//   - lowercase + mixed case + digits + special chars (no
//     restriction; the env-var-key regex from SealOne does NOT apply)
//   - empty plaintext (boundary)
//   - binary-ish bytes (no NUL inside; the tag terminator is at
//     index 0 and the plaintext starts at index 1)
func TestSealBytesOpenBytesRoundTrip(t *testing.T) {
	id := mustGenHostKey(t, "host.age")
	cases := []struct {
		name      string
		namespace string
		plaintext []byte
	}{
		{"lowercase_namespace", "alert_rule_secret", []byte("plaintext-shh")},
		{"empty_namespace", "", []byte("plaintext-shh")},
		{"empty_plaintext", "alert_rule_secret", []byte{}},
		{"binary_payload", "alert_rule_secret", []byte{0x00, 0x01, 0x02, 0xff, 0xfe, 0xfd}},
		{"base64_secret", "alert_rule_secret", []byte("aGVsbG8td29ybGQ=")},
	}
	for _, tc := range cases {
		blob, err := SealBytes(id.Recipient(), tc.namespace, tc.plaintext, 256)
		if err != nil {
			t.Fatalf("SealBytes(%q): %v", tc.namespace, err)
		}
		gotNS, gotPlain, err := OpenBytes(id, blob)
		if err != nil {
			t.Fatalf("OpenBytes after SealBytes(%q): %v", tc.namespace, err)
		}
		if gotNS != tc.namespace {
			t.Errorf("namespace round-trip: got %q, want %q", gotNS, tc.namespace)
		}
		if !bytes.Equal(gotPlain, tc.plaintext) {
			t.Errorf("plaintext round-trip: got %v, want %v", gotPlain, tc.plaintext)
		}
	}
}

// TestSealBytesEnforcesByteCap asserts the byte cap is enforced
// at the seal boundary (defense in depth — the handler also
// enforces). A 64-byte plaintext with a 32-byte cap must fail.
func TestSealBytesEnforcesByteCap(t *testing.T) {
	id := mustGenHostKey(t, "host.age")
	_, err := SealBytes(id.Recipient(), "alert_rule_secret", bytes.Repeat([]byte("a"), 64), 32)
	if err == nil {
		t.Fatal("SealBytes accepted over-cap plaintext")
	}
}

// TestSealBytes_NilRecipient_ReturnsError pins the nil-recipient
// 503 path. PR review finding F8: the handler tests use
// `withTestRecipient(t)` to avoid this path, leaving it untested.
// A future refactor that breaks recipient wiring could silently
// regress callers — the handler falls through to ErrCapacity on
// this error, so we want to be sure the error is non-nil.
func TestSealBytes_NilRecipient_ReturnsError(t *testing.T) {
	_, err := SealBytes(nil, "alert_rule_secret", []byte("plaintext"), 256)
	if err == nil {
		t.Fatal("SealBytes with nil recipient returned nil error")
	}
}

// TestOpenBytes_NilIdentity_ReturnsError pins the nil-identity
// mirror of the above for the OpenBytes path.
func TestOpenBytes_NilIdentity_ReturnsError(t *testing.T) {
	_, _, err := OpenBytes(nil, []byte("anything"))
	if err == nil {
		t.Fatal("OpenBytes with nil identity returned nil error")
	}
}

// TestOpenBytes_EmptyBlob_ReturnsError pins the empty-blob guard.
// Decrypting an empty age stream is undefined behaviour; the
// handler must reject it before age even tries.
func TestOpenBytes_EmptyBlob_ReturnsError(t *testing.T) {
	id := mustGenHostKey(t, "host.age")
	_, _, err := OpenBytes(id, nil)
	if err == nil {
		t.Fatal("OpenBytes with empty blob returned nil error")
	}
	_, _, err = OpenBytes(id, []byte{})
	if err == nil {
		t.Fatal("OpenBytes with empty blob returned nil error")
	}
}

// TestOpenMulti_CurrentAndPrevious is the load-bearing test for
// the rotation-overlap plumbing (issue #316 / ADR-057). We seal
// against identity A, then unseal with a slice that contains a
// DIFFERENT identity B in slot 0 AND A in slot 1 — both orderings
// must succeed because age.Decrypt falls back across the slice.
//
// This pins the contract: after `gregale host-age rotate --commit`,
// envelopes sealed under the PREVIOUS host.age are still unsealable
// without operator intervention, until prune-previous is invoked.
func TestOpenMulti_CurrentAndPrevious(t *testing.T) {
	prevID := mustGenHostKey(t, "prev.age")
	currID := mustGenHostKey(t, "curr.age")

	blob, err := Seal(prevID.Recipient(), Envelope{"API_KEY": "secret-value"})
	if err != nil {
		t.Fatalf("Seal under prevID: %v", err)
	}

	// Order 1: current first, previous second — matches LoadHostKeys output.
	env, err := OpenMulti([]*age.X25519Identity{currID, prevID}, blob)
	if err != nil {
		t.Fatalf("OpenMulti [curr,prev]: %v", err)
	}
	if env["API_KEY"] != "secret-value" {
		t.Errorf("decrypted value mismatch: got %q", env["API_KEY"])
	}

	// Order 2: previous first — must also work because the fallback
	// is across the slice, not position-dependent.
	env, err = OpenMulti([]*age.X25519Identity{prevID, currID}, blob)
	if err != nil {
		t.Fatalf("OpenMulti [prev,curr]: %v", err)
	}
	if env["API_KEY"] != "secret-value" {
		t.Errorf("decrypted value mismatch (order 2): got %q", env["API_KEY"])
	}
}

// TestOpenMulti_SingleIdentity mirrors Open's behaviour for the
// single-identity case. The 1-element-slice path is a hot path
// (every unseal during the rotation overlap actually hits it
// when the envelope was sealed under the current key — the
// fallback only fires for the previous-keyed envelopes).
func TestOpenMulti_SingleIdentity(t *testing.T) {
	id := mustGenHostKey(t, "host.age")
	blob, err := Seal(id.Recipient(), Envelope{"K": "v"})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	got, err := OpenMulti([]*age.X25519Identity{id}, blob)
	if err != nil {
		t.Fatalf("OpenMulti: %v", err)
	}
	if got["K"] != "v" {
		t.Errorf("got %q, want \"v\"", got["K"])
	}
}

// TestOpenMulti_AllWrong confirms the multi-identity contract still
// fails loudly when no supplied identity matches. Pin the
// non-nil error shape so a regression to "silently return zero
// Envelope" is caught loudly.
func TestOpenMulti_AllWrong(t *testing.T) {
	a := mustGenHostKey(t, "a.age")
	b := mustGenHostKey(t, "b.age")
	blob, err := Seal(a.Recipient(), Envelope{"K": "v"})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// b + an unrelated c — neither matches the seal under a.
	c := mustGenHostKey(t, "c.age")
	if _, err := OpenMulti([]*age.X25519Identity{b, c}, blob); err == nil {
		t.Fatal("OpenMulti with all-wrong identities succeeded")
	}
}

// TestOpenMulti_EmptyInputs pins the precondition errors. An empty
// slice or empty blob must surface before age is called.
func TestOpenMulti_EmptyInputs(t *testing.T) {
	if _, err := OpenMulti(nil, []byte("anything")); err == nil {
		t.Error("OpenMulti(nil identities): expected error")
	}
	if _, err := OpenMulti([]*age.X25519Identity{}, []byte("anything")); err == nil {
		t.Error("OpenMulti(empty slice): expected error")
	}
	if _, err := OpenMulti([]*age.X25519Identity{(*age.X25519Identity)(nil)}, nil); err == nil {
		t.Error("OpenMulti(empty blob): expected error")
	}
}

// TestOpenMulti_NilElementInSlice pins the nil-element defence
// (PR #487 review finding #3). age.Decrypt panics on a nil
// *age.X25519Identity (x25519.go:158 Unwrap dereferences), so
// OpenMulti must filter silent nils before widening to the
// []age.Identity interface slice. The widening is "one-sided"
// in the sense that the nil-filter must happen BEFORE the
// []age.Identity conversion — the test covers both the
// "all-nil slice" guard and the "mixed nil + valid" path
// (the latter must NOT panic and must succeed via the valid
// identity).
func TestOpenMulti_NilElementInSlice(t *testing.T) {
	id := mustGenHostKey(t, "host.age")
	blob, err := Seal(id.Recipient(), Envelope{"K": "v"})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// All-nil slice: must return an error (not panic).
	if _, err := OpenMulti([]*age.X25519Identity{nil, nil, nil}, blob); err == nil {
		t.Error("OpenMulti(all-nil identities): expected error, got nil")
	}

	// Mixed nil + valid: must succeed via the valid identity, NOT panic.
	got, err := OpenMulti([]*age.X25519Identity{nil, id, nil}, blob)
	if err != nil {
		t.Fatalf("OpenMulti(mixed nil + valid): %v", err)
	}
	if got["K"] != "v" {
		t.Errorf("decryption mismatch: got %q, want \"v\"", got["K"])
	}

	// Trailing-nil: must succeed via the leading valid identity.
	got, err = OpenMulti([]*age.X25519Identity{id, nil}, blob)
	if err != nil {
		t.Fatalf("OpenMulti([id, nil]): %v", err)
	}
	if got["K"] != "v" {
		t.Errorf("decryption mismatch: got %q, want \"v\"", got["K"])
	}
}

// TestOpenBytesMulti_NilElementInSlice mirrors the byte-channel
// counterpart of TestOpenMulti_NilElementInSlice.
func TestOpenBytesMulti_NilElementInSlice(t *testing.T) {
	id := mustGenHostKey(t, "host.age")
	blob, err := SealBytes(id.Recipient(), "alert_rule_secret", []byte("plaintext"), 256)
	if err != nil {
		t.Fatalf("SealBytes: %v", err)
	}

	if _, _, err := OpenBytesMulti([]*age.X25519Identity{nil, nil, nil}, blob); err == nil {
		t.Error("OpenBytesMulti(all-nil identities): expected error, got nil")
	}

	gotNS, gotPlain, err := OpenBytesMulti([]*age.X25519Identity{nil, id, nil}, blob)
	if err != nil {
		t.Fatalf("OpenBytesMulti(mixed nil + valid): %v", err)
	}
	if gotNS != "alert_rule_secret" {
		t.Errorf("namespace: got %q, want \"alert_rule_secret\"", gotNS)
	}
	if string(gotPlain) != "plaintext" {
		t.Errorf("plaintext: got %q, want \"plaintext\"", gotPlain)
	}
}

// TestOpenMulti_RotateEndToEnd pins the full rotation round-trip
// (PR #487 review finding #15). Mirrors what `gregale host-age
// rotate --commit` does on disk:
//
//  1. Init a fresh dir with host.age (identity A).
//  2. Seal an envelope under A — this is the "pre-rotation customer
//     secret" the box needs to keep reading for 30 days.
//  3. Move host.age → host.age.previous (still identity A).
//  4. Generate a fresh identity B, write as host.age.
//  5. LoadHostKeys(dir) returns [B, A].
//  6. OpenMulti([B, A], pre-rotation-blob) succeeds via fallback to A.
//  7. OpenMulti([B, A], new-blob-sealed-under-B) succeeds.
//  8. After prune-previous (host.age.previous removed), OpenMulti
//     returns the all-nil-previous → still 1-element [B] case and
//     unseals the B-sealed blob; the A-sealed blob now fails.
//  9. LoadHostKeys(dir) returns just [B] post-prune.
//
// This is the contract that closes the gap the runbook promises:
// "rotate does not strand pre-rotation sealed secrets."
func TestOpenMulti_RotateEndToEnd(t *testing.T) {
	dir := t.TempDir()

	// (1) Seed host.age as identity A.
	idA, err := GenerateAndSaveHostKey(filepath.Join(dir, "host.age"))
	if err != nil {
		t.Fatalf("seed host.age: %v", err)
	}

	// (2) Pre-rotation seal under A.
	preBlob, err := Seal(idA.Recipient(), Envelope{"STRIPE_KEY": "sk_live_old"})
	if err != nil {
		t.Fatalf("seal under A: %v", err)
	}

	// (3) Move host.age → host.age.previous.
	if err := os.Rename(filepath.Join(dir, "host.age"), filepath.Join(dir, "host.age.previous")); err != nil {
		t.Fatalf("rename current → previous: %v", err)
	}

	// (4) Generate identity B, save as the new current.
	idB, err := GenerateAndSaveHostKey(filepath.Join(dir, "host.age"))
	if err != nil {
		t.Fatalf("seed new host.age: %v", err)
	}
	if idA.Recipient().String() == idB.Recipient().String() {
		t.Fatal("rotation produced identical recipient — RNG broken?")
	}

	// (5) LoadHostKeys returns [B, A].
	loaded, err := LoadHostKeys(dir)
	if err != nil {
		t.Fatalf("LoadHostKeys: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("LoadHostKeys len=%d, want 2 (current + previous)", len(loaded))
	}
	if loaded[0].Recipient().String() != idB.Recipient().String() {
		t.Errorf("loaded[0] is the OLD identity, want new current")
	}
	if loaded[1].Recipient().String() != idA.Recipient().String() {
		t.Errorf("loaded[1] is the NEW identity, want previous (identity A)")
	}

	// (6) OpenMulti falls back to A — pre-rotation envelope survives.
	got, err := OpenMulti(loaded, preBlob)
	if err != nil {
		t.Fatalf("OpenMulti of pre-rotation blob via [B,A]: %v", err)
	}
	if got["STRIPE_KEY"] != "sk_live_old" {
		t.Errorf("pre-rotation unseal: got %q, want \"sk_live_old\"", got["STRIPE_KEY"])
	}

	// (7) A new envelope sealed under B (post-rotation write) opens too.
	postBlob, err := Seal(idB.Recipient(), Envelope{"STRIPE_KEY": "sk_live_new"})
	if err != nil {
		t.Fatalf("seal under B: %v", err)
	}
	got, err = OpenMulti(loaded, postBlob)
	if err != nil {
		t.Fatalf("OpenMulti of post-rotation blob via [B,A]: %v", err)
	}
	if got["STRIPE_KEY"] != "sk_live_new" {
		t.Errorf("post-rotation unseal: got %q, want \"sk_live_new\"", got["STRIPE_KEY"])
	}

	// Sanity: B alone cannot unseal the A-sealed blob (recipient binding is real).
	if _, err := OpenMulti([]*age.X25519Identity{idB}, preBlob); err == nil {
		t.Error("OpenMulti[B] of A-sealed blob succeeded — recipient binding broken")
	}

	// (8) Prune-previous: remove .previous.
	if err := os.Remove(filepath.Join(dir, "host.age.previous")); err != nil {
		t.Fatalf("remove .previous: %v", err)
	}

	// (9) LoadHostKeys now returns just [B].
	loadedAfterPrune, err := LoadHostKeys(dir)
	if err != nil {
		t.Fatalf("LoadHostKeys post-prune: %v", err)
	}
	if len(loadedAfterPrune) != 1 {
		t.Fatalf("LoadHostKeys post-prune len=%d, want 1", len(loadedAfterPrune))
	}
	if loadedAfterPrune[0].Recipient().String() != idB.Recipient().String() {
		t.Errorf("post-prune current identity mismatch")
	}

	// Post-prune: B-sealed blob still works; A-sealed blob now fails.
	if _, err := OpenMulti(loadedAfterPrune, postBlob); err != nil {
		t.Errorf("post-prune unseal of B-sealed blob failed: %v", err)
	}
	if _, err := OpenMulti(loadedAfterPrune, preBlob); err == nil {
		t.Error("post-prune unseal of A-sealed blob succeeded — overlap should be over")
	}
}

// TestOpenBytesMulti_RotateEndToEnd is the byte-channel counterpart
// of TestOpenMulti_RotateEndToEnd — the alert evaluator webhook
// secret path that drove the original ADR-057 motivation.
func TestOpenBytesMulti_RotateEndToEnd(t *testing.T) {
	dir := t.TempDir()

	idA, err := GenerateAndSaveHostKey(filepath.Join(dir, "host.age"))
	if err != nil {
		t.Fatalf("seed host.age: %v", err)
	}
	preBlob, err := SealBytes(idA.Recipient(), "alert_rule_secret", []byte("https://hooks.old/path"), 256)
	if err != nil {
		t.Fatalf("SealBytes under A: %v", err)
	}

	if err := os.Rename(filepath.Join(dir, "host.age"), filepath.Join(dir, "host.age.previous")); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, err := GenerateAndSaveHostKey(filepath.Join(dir, "host.age")); err != nil {
		t.Fatalf("seed new host.age: %v", err)
	}

	loaded, err := LoadHostKeys(dir)
	if err != nil {
		t.Fatalf("LoadHostKeys: %v", err)
	}
	gotNS, gotPlain, err := OpenBytesMulti(loaded, preBlob)
	if err != nil {
		t.Fatalf("OpenBytesMulti fallback to A: %v", err)
	}
	if gotNS != "alert_rule_secret" {
		t.Errorf("namespace mismatch: got %q", gotNS)
	}
	if string(gotPlain) != "https://hooks.old/path" {
		t.Errorf("plaintext mismatch: got %q", gotPlain)
	}
}

// TestOpenMulti_RoundTrip is the byte-channel counterpart of
// TestOpenMulti_CurrentAndPrevious. Confirms the alert evaluator
// (pkg/alerts/evaluator.go) can keep unsealing webhook secrets
// sealed under the previous host.age across a rotate.
func TestOpenBytesMulti_RoundTrip(t *testing.T) {
	prevID := mustGenHostKey(t, "prev.age")
	currID := mustGenHostKey(t, "curr.age")
	const ns = "alert_rule_secret"
	const plaintext = "https://hooks.example.com/path"

	blob, err := SealBytes(prevID.Recipient(), ns, []byte(plaintext), 256)
	if err != nil {
		t.Fatalf("SealBytes under prevID: %v", err)
	}

	gotNS, gotPlain, err := OpenBytesMulti([]*age.X25519Identity{currID, prevID}, blob)
	if err != nil {
		t.Fatalf("OpenBytesMulti: %v", err)
	}
	if gotNS != ns {
		t.Errorf("namespace: got %q, want %q", gotNS, ns)
	}
	if string(gotPlain) != plaintext {
		t.Errorf("plaintext: got %q, want %q", gotPlain, plaintext)
	}
}

// TestOpenBytesMulti_EmptyInputs mirrors TestOpenMulti_EmptyInputs
// for the byte channel.
func TestOpenBytesMulti_EmptyInputs(t *testing.T) {
	if _, _, err := OpenBytesMulti(nil, []byte("anything")); err == nil {
		t.Error("OpenBytesMulti(nil identities): expected error")
	}
	if _, _, err := OpenBytesMulti([]*age.X25519Identity{}, []byte("anything")); err == nil {
		t.Error("OpenBytesMulti(empty slice): expected error")
	}
}

// --- helpers ---------------------------------------------------------------

// envelopesEqual compares two Envelopes without relying on map ordering.
func envelopesEqual(a, b Envelope) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// mustGenHostKey generates a host key in a per-test temp dir and returns the
// identity. Failure is fatal — every test that uses it relies on the key
// being usable.
func mustGenHostKey(t *testing.T, name string) *age.X25519Identity {
	t.Helper()
	id, err := GenerateAndSaveHostKey(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	return id
}

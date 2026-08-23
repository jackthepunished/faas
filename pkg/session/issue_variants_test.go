// issue_variants_test.go — fill pkg/session coverage of the
// envelope variant helpers and BindingKey that round-trip only the
// envelope fields they were designed for.
//
// Targets:
//   - BindingKey: nil receiver, populated 32-byte key, defensive-copy
//     isolation, key differs across managers
//   - NewEphemeralManager: maxAge<=0 falls through to 7d default
//   - IssueWithSessionAndBindingHash: round-trip the binding_hash
//     field; empty binding_hash produces no JSON key (back-compat)
//   - IssueWithSessionAndBindingHashAndStepUp: round-trip step_up_at;
//     zero step_up_at produces no JSON key (back-compat)
//   - IssueWithSessionAndGithubLogin: round-trip the github_login;
//     empty login produces no JSON key (back-compat)
//   - IssueWithSessionAndGithubLoginAndBindingHash: round-trip the
//     union of github_login + binding_hash + sid + mfa_pending
//   - SealGithubLogin: nonce randomness + empty github_login omitempty

package session_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/session"
)

// --- BindingKey ----------------------------------------------------

// TestBindingKey_NilReceiverReturnsNil covers the explicit nil-receiver
// guard at manager.go:114-115. The Manager is held by the apid
// middleware as a pointer; a future code path that dials it before
// init must get a nil slice back, not a panic from a missing-key check.
func TestBindingKey_NilReceiverReturnsNil(t *testing.T) {
	var m *session.Manager
	if got := m.BindingKey(); got != nil {
		t.Errorf("nil receiver: got %v, want nil", got)
	}
}

// TestBindingKey_ZeroValueManagerReturnsNil covers the
// `m.bindingKey == nil` branch at manager.go:115. A zero-value
// Manager (constructed with &Manager{} and no NewManager call) must
// return nil from BindingKey rather than handing out a nil-slice
// reference the caller might copy.
func TestBindingKey_ZeroValueManagerReturnsNil(t *testing.T) {
	m := &session.Manager{}
	if got := m.BindingKey(); got != nil {
		t.Errorf("zero-value: got %v, want nil", got)
	}
}

// TestBindingKey_Returns32Bytes pins the documented contract from
// the package comment: the returned slice is freshly allocated and
// is exactly 32 bytes long. ADR-076's bindinghash.Compute HMACs
// with this key; a length drift would surface as a hash mismatch,
// not as a compile error.
func TestBindingKey_Returns32Bytes(t *testing.T) {
	m, err := session.NewManager(key(t), time.Hour)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	got := m.BindingKey()
	if len(got) != 32 {
		t.Errorf("len = %d, want 32", len(got))
	}
}

// TestBindingKey_DefensiveCopyIsolatesMutation covers the contract
// that mutating the returned slice does NOT mutate the Manager's
// internal bindingKey. A future refactor that returns the field
// directly (instead of copying) would break callers who zero out
// the returned slice as a best practice.
func TestBindingKey_DefensiveCopyIsolatesMutation(t *testing.T) {
	m, err := session.NewManager(key(t), time.Hour)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	a := m.BindingKey()
	for i := range a {
		a[i] = 0xAA
	}
	b := m.BindingKey()
	for i, v := range b {
		if v == 0xAA {
			t.Errorf("mutation leaked at index %d: got 0x%02x", i, v)
			break
		}
	}
}

// TestBindingKey_DiffersAcrossManagers confirms two independent
// NewManager calls (with different keys) produce different
// BindingKey outputs. A regression that returned a constant or
// shared global would make ADR-076's HMAC fingerprint useless:
// every customer would be cross-correlatable.
func TestBindingKey_DiffersAcrossManagers(t *testing.T) {
	m1, err := session.NewManager(key(t), time.Hour)
	if err != nil {
		t.Fatalf("NewManager#1: %v", err)
	}
	other := make([]byte, 32)
	for i := range other {
		other[i] = byte(0xFF - i)
	}
	m2, err := session.NewManager(other, time.Hour)
	if err != nil {
		t.Fatalf("NewManager#2: %v", err)
	}
	a := m1.BindingKey()
	b := m2.BindingKey()
	if bytes.Equal(a, b) {
		t.Errorf("BindingKey equal across distinct managers; expected distinct key bytes")
	}
}

// TestBindingKey_StableAcrossCalls pins the side-by-side invariant:
// the same Manager returns the same key bytes on every call. The
// defensive copy in BindingKey must produce identical output, not
// randomized bytes (which would defeat ADR-076's HMAC).
func TestBindingKey_StableAcrossCalls(t *testing.T) {
	m, err := session.NewManager(key(t), time.Hour)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	a := m.BindingKey()
	b := m.BindingKey()
	if !bytes.Equal(a, b) {
		t.Errorf("BindingKey is not stable across calls")
	}
}

// --- NewEphemeralManager: maxAge default ----------------------------

// TestNewEphemeralManager_ZeroMaxAgeDefaults exercises the
// `if maxAge <= 0` default branch (manager.go:78-80) via the
// ephemeral constructor. The dev-mode path passes 0 to mean "use
// the production default"; without this test the fallback only
// fires for callers of NewManager with a literal key.
func TestNewEphemeralManager_ZeroMaxAgeDefaults(t *testing.T) {
	m, err := session.NewEphemeralManager(0)
	if err != nil {
		t.Fatalf("NewEphemeralManager(0): %v", err)
	}
	if got := m.MaxAge(); got != 7*24*time.Hour {
		t.Errorf("MaxAge() = %v, want 7d default", got)
	}
}

func TestNewEphemeralManager_NegativeMaxAgeDefaults(t *testing.T) {
	m, err := session.NewEphemeralManager(-time.Hour)
	if err != nil {
		t.Fatalf("NewEphemeralManager(-1h): %v", err)
	}
	if got := m.MaxAge(); got != 7*24*time.Hour {
		t.Errorf("MaxAge() = %v, want 7d default", got)
	}
}

// TestNewEphemeralManager_KeyCopiedFromRandReader covers the
// `io.ReadFull(rand.Reader, key)` branch (manager.go:189-191) by
// asserting two ephemerals produce distinct BindingKey outputs.
// A regression that didn't actually read from rand.Reader would
// hand every ephemeral manager the same key, defeating the
// dev-mode "every restart invalidates every cookie" contract.
func TestNewEphemeralManager_KeyCopiedFromRandReader(t *testing.T) {
	m1, err := session.NewEphemeralManager(time.Hour)
	if err != nil {
		t.Fatalf("NewEphemeralManager#1: %v", err)
	}
	m2, err := session.NewEphemeralManager(time.Hour)
	if err != nil {
		t.Fatalf("NewEphemeralManager#2: %v", err)
	}
	a := m1.BindingKey()
	b := m2.BindingKey()
	if bytes.Equal(a, b) {
		t.Errorf("two ephemeral managers produced identical keys; rand.Reader not consulted")
	}
}

// --- IssueWithSessionAndBindingHash --------------------------------

// TestIssueWithSessionAndBindingHash_RoundTrip covers the
// binding-hash-stamping path used by the dashboard /v1/account/mfa
// login flow. The ADR-076 fingerprint must round-trip through
// Verify so the cookie and the sessions row agree on the
// fingerprint hash; a regression would trip the requireSession
// 401 branch on every fresh login.
func TestIssueWithSessionAndBindingHash_RoundTrip(t *testing.T) {
	m, err := session.NewManager(key(t), time.Hour)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	const (
		sid         = "22222222-2222-2222-2222-222222222222"
		bindingHash = "deadbeef-0000-0000-0000-000000000000"
	)
	v, err := m.IssueWithSessionAndBindingHash(sid, "acct-bh", bindingHash, true)
	if err != nil {
		t.Fatalf("IssueWithSessionAndBindingHash: %v", err)
	}
	env, err := m.Verify(v)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if env.AccountID != "acct-bh" {
		t.Errorf("AccountID = %q, want acct-bh", env.AccountID)
	}
	if env.Sid != sid {
		t.Errorf("Sid = %q, want %q", env.Sid, sid)
	}
	if env.BindingHash != bindingHash {
		t.Errorf("BindingHash = %q, want %q", env.BindingHash, bindingHash)
	}
	if !env.MfaPending {
		t.Errorf("MfaPending = false, want true")
	}
}

// TestIssueWithSessionAndBindingHash_EmptyBindingHashOmits covers
// the back-compat property documented at manager.go:336-340 — an
// empty BindingHash field does not appear on the wire (omitempty),
// and Verify still decodes with BindingHash == "". The cross-check
// in requireSession skips when the field is empty.
func TestIssueWithSessionAndBindingHash_EmptyBindingHashOmits(t *testing.T) {
	m, err := session.NewManager(key(t), time.Hour)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	v, err := m.IssueWithSessionAndBindingHash("s-1", "acct-bh", "", false)
	if err != nil {
		t.Fatalf("IssueWithSessionAndBindingHash: %v", err)
	}
	env, err := m.Verify(v)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if env.BindingHash != "" {
		t.Errorf("BindingHash = %q, want empty (omitempty)", env.BindingHash)
	}
	if env.MfaPending {
		t.Errorf("MfaPending = true, want false")
	}
}

// --- IssueWithSessionAndBindingHashAndStepUp ----------------------

// TestIssueWithSessionAndBindingHashAndStepUp_RoundTrip covers the
// ADR-077 step-up + ADR-076 binding union path. The step_up_at
// timestamp must round-trip; a regression would silently let
// requireStepUp downgrade to the pre-077 always-allow branch.
func TestIssueWithSessionAndBindingHashAndStepUp_RoundTrip(t *testing.T) {
	m, err := session.NewManager(key(t), time.Hour)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	const (
		sid         = "33333333-3333-3333-3333-333333333333"
		bindingHash = "step-up-bh-marker"
	)
	stepUp := time.Unix(1_734_567_890, 0).UTC()
	v, err := m.IssueWithSessionAndBindingHashAndStepUp(
		sid, "acct-su", bindingHash, stepUp, false,
	)
	if err != nil {
		t.Fatalf("IssueWithSessionAndBindingHashAndStepUp: %v", err)
	}
	env, err := m.Verify(v)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if env.AccountID != "acct-su" {
		t.Errorf("AccountID = %q", env.AccountID)
	}
	if env.BindingHash != bindingHash {
		t.Errorf("BindingHash = %q, want %q", env.BindingHash, bindingHash)
	}
	if !env.StepUpAt.Equal(stepUp) {
		t.Errorf("StepUpAt = %v, want %v", env.StepUpAt, stepUp)
	}
	if env.Sid != sid {
		t.Errorf("Sid = %q, want %q", env.Sid, sid)
	}
}

// TestIssueWithSessionAndBindingHashAndStepUp_ZeroStepUpOmits
// covers the omitempty contract for step_up_at: a zero timestamp
// does not appear on the wire, and Verify decodes the field to
// the zero time. Pre-PR-077 cookies therefore decode unchanged.
func TestIssueWithSessionAndBindingHashAndStepUp_ZeroStepUpOmits(t *testing.T) {
	m, err := session.NewManager(key(t), time.Hour)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	v, err := m.IssueWithSessionAndBindingHashAndStepUp(
		"s-1", "acct-su", "bh", time.Time{}, false,
	)
	if err != nil {
		t.Fatalf("IssueWithSessionAndBindingHashAndStepUp: %v", err)
	}
	env, err := m.Verify(v)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !env.StepUpAt.IsZero() {
		t.Errorf("StepUpAt = %v, want zero (omitempty)", env.StepUpAt)
	}
}

// --- IssueWithSessionAndGithubLogin --------------------------------

// TestIssueWithSessionAndGithubLogin_RoundTrip covers the §11
// ownership proof path: a fresh dashboard cookie carries
// github_login through to Verify. The dashboard chrome reads the
// field on every page load to render "signed in as <login>".
func TestIssueWithSessionAndGithubLogin_RoundTrip(t *testing.T) {
	m, err := session.NewManager(key(t), time.Hour)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	const (
		sid  = "44444444-4444-4444-4444-444444444444"
		gh   = "octocat"
		acct = "acct-gh"
	)
	v, err := m.IssueWithSessionAndGithubLogin(sid, acct, gh, true)
	if err != nil {
		t.Fatalf("IssueWithSessionAndGithubLogin: %v", err)
	}
	env, err := m.Verify(v)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if env.AccountID != acct {
		t.Errorf("AccountID = %q, want %q", env.AccountID, acct)
	}
	if env.Sid != sid {
		t.Errorf("Sid = %q, want %q", env.Sid, sid)
	}
	if env.GithubLogin != gh {
		t.Errorf("GithubLogin = %q, want %q", env.GithubLogin, gh)
	}
	if !env.MfaPending {
		t.Errorf("MfaPending = false, want true")
	}
}

// TestIssueWithSessionAndGithubLogin_EmptyOmits covers the
// omitempty back-compat: a cookie sealed without a github_login
// must decode with GithubLogin == "" and produce a wire envelope
// byte-for-byte compatible with the pre-PR-B shape.
func TestIssueWithSessionAndGithubLogin_EmptyOmits(t *testing.T) {
	m, err := session.NewManager(key(t), time.Hour)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	v, err := m.IssueWithSessionAndGithubLogin("s-1", "acct-gh", "", false)
	if err != nil {
		t.Fatalf("IssueWithSessionAndGithubLogin: %v", err)
	}
	env, err := m.Verify(v)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if env.GithubLogin != "" {
		t.Errorf("GithubLogin = %q, want empty (omitempty)", env.GithubLogin)
	}
	// Re-marshaling the verified envelope must NOT carry the
	// github_login key (omitempty contract).
	reMarshal, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(reMarshal), "github_login") {
		t.Errorf("empty GithubLogin re-marshaled to %q; omitempty broken", reMarshal)
	}
}

// --- IssueWithSessionAndGithubLoginAndBindingHash ------------------

// TestIssueWithSessionAndGithubLoginAndBindingHash_RoundTrip covers
// the §11 / ADR-076 union: github_login + binding_hash land on the
// same envelope in a single seal. The /v1/auth/github handler is
// the only path that stamps all three (sid + binding + github);
// without this test the union branch was uncovered.
func TestIssueWithSessionAndGithubLoginAndBindingHash_RoundTrip(t *testing.T) {
	m, err := session.NewManager(key(t), time.Hour)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	const (
		sid         = "55555555-5555-5555-5555-555555555555"
		gh          = "monalisa"
		bindingHash = "gh-binding-marker"
	)
	v, err := m.IssueWithSessionAndGithubLoginAndBindingHash(
		sid, "acct-union", gh, bindingHash, false,
	)
	if err != nil {
		t.Fatalf("IssueWithSessionAndGithubLoginAndBindingHash: %v", err)
	}
	env, err := m.Verify(v)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if env.AccountID != "acct-union" {
		t.Errorf("AccountID = %q", env.AccountID)
	}
	if env.Sid != sid {
		t.Errorf("Sid = %q, want %q", env.Sid, sid)
	}
	if env.GithubLogin != gh {
		t.Errorf("GithubLogin = %q, want %q", env.GithubLogin, gh)
	}
	if env.BindingHash != bindingHash {
		t.Errorf("BindingHash = %q, want %q", env.BindingHash, bindingHash)
	}
	if env.MfaPending {
		t.Errorf("MfaPending = true, want false")
	}
}

// TestIssueWithSessionAndGithubLoginAndBindingHash_BothEmptyOmits
// covers the documented "cross-check skips when both empty" path
// at manager.go:288-289. Both optional fields omitted from the
// wire envelope; Verify decodes cleanly with both at the zero
// value.
func TestIssueWithSessionAndGithubLoginAndBindingHash_BothEmptyOmits(t *testing.T) {
	m, err := session.NewManager(key(t), time.Hour)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	v, err := m.IssueWithSessionAndGithubLoginAndBindingHash(
		"s-1", "acct-union", "", "", false,
	)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	env, err := m.Verify(v)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if env.GithubLogin != "" || env.BindingHash != "" {
		t.Errorf("got (%q, %q), want both empty", env.GithubLogin, env.BindingHash)
	}
}

// --- SealGithubLogin nonce randomness ------------------------------

// TestSealGithubLogin_NonceRandomness covers the
// `io.ReadFull(rand.Reader, nonce)` branch at manager.go:410-412.
// Two seals with identical inputs must produce different base64
// outputs. A regression that reused the nonce would let an
// attacker replay or correlate /v1/auth/github tokens.
func TestSealGithubLogin_NonceRandomness(t *testing.T) {
	m, err := session.NewManager(key(t), time.Hour)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	a, err := m.SealGithubLogin("acct-1", "octocat", false)
	if err != nil {
		t.Fatalf("SealGithubLogin #1: %v", err)
	}
	b, err := m.SealGithubLogin("acct-1", "octocat", false)
	if err != nil {
		t.Fatalf("SealGithubLogin #2: %v", err)
	}
	if a == b {
		t.Errorf("two seals of identical inputs produced identical base64; nonce is being reused")
	}
}

// TestSealGithubLogin_EmptyGithubLoginOmits covers the omitempty
// back-compat for the GithubLogin field on the SealGithubLogin
// path. A caller who passes "" must produce a wire envelope
// indistinguishable from the pre-PR-B shape (which had no
// github_login field).
func TestSealGithubLogin_EmptyGithubLoginOmits(t *testing.T) {
	m, err := session.NewManager(key(t), time.Hour)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	v, err := m.SealGithubLogin("acct-gh-empty", "", false)
	if err != nil {
		t.Fatalf("SealGithubLogin: %v", err)
	}
	env, err := m.Verify(v)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if env.GithubLogin != "" {
		t.Errorf("GithubLogin = %q, want empty", env.GithubLogin)
	}
}

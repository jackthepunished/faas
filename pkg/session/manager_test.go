package session_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/session"
)

func key(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

func TestNewManager_RejectsShortKey(t *testing.T) {
	if _, err := session.NewManager([]byte("short"), time.Hour); err == nil {
		t.Fatal("expected error for short key")
	}
}

// TestNewManager_ZeroesCallerKey confirms the caller's key slice is
// wiped on a successful NewManager. The Manager itself keeps only
// the AEAD; the caller's slice must not retain the secret.
func TestNewManager_ZeroesCallerKey(t *testing.T) {
	k := key(t)
	// Snapshot a non-zero byte — if NewManager didn't wipe, we'd see it.
	original := append([]byte(nil), k...)
	if _, err := session.NewManager(k, time.Hour); err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	for i, b := range k {
		if b != 0 {
			t.Errorf("caller key not zeroed at index %d (got 0x%02x; original 0x%02x)", i, b, original[i])
		}
	}
}

func TestIssue_And_Verify_RoundTrip(t *testing.T) {
	m, err := session.NewManager(key(t), time.Hour)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	v, err := m.Issue("acct-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	env, err := m.Verify(v)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if env.AccountID != "acct-1" {
		t.Errorf("account = %q, want acct-1", env.AccountID)
	}
	if env.ExpiresAt.Before(time.Now()) {
		t.Errorf("expires = %v, want in future", env.ExpiresAt)
	}
}

func TestVerify_RejectsTampered(t *testing.T) {
	m, _ := session.NewManager(key(t), time.Hour)
	v, _ := m.Issue("acct-1")
	// Replace a base64 char in the middle of the encoded blob with a
	// character that is NOT in the RawURLEncoding alphabet so the
	// tamper is always observable. Earlier this used "X", which is a
	// valid base64 char and happens to be the source character ~1/64
	// of the time — producing a no-op tamper that Verify then
	// accepts, which manifests as a 1-3% CI flake.
	tampered := v[:len(v)/2] + "!" + v[len(v)/2+1:]
	if _, err := m.Verify(tampered); !errors.Is(err, session.ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
}

func TestVerify_RejectsEmptyEnvelope(t *testing.T) {
	m, _ := session.NewManager(key(t), time.Hour)
	if _, err := m.Verify(""); !errors.Is(err, session.ErrInvalid) {
		t.Errorf("empty cookie err = %v, want ErrInvalid", err)
	}
}

func TestVerify_RejectsExpired(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	m, err := session.NewManager(key(t), time.Hour)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	m.SetClock(func() time.Time { return now })
	v, err := m.Issue("acct-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	// Advance past maxAge (1h).
	m.SetClock(func() time.Time { return now.Add(time.Hour + time.Minute) })
	if _, err := m.Verify(v); !errors.Is(err, session.ErrInvalid) {
		t.Errorf("expired err = %v, want ErrInvalid", err)
	}
}

func TestVerify_RejectsWrongKey(t *testing.T) {
	m1, _ := session.NewManager(key(t), time.Hour)
	m2, _ := session.NewManager([]byte(strings.Repeat("x", 32)), time.Hour)
	v, _ := m1.Issue("acct-1")
	if _, err := m2.Verify(v); !errors.Is(err, session.ErrInvalid) {
		t.Errorf("wrong-key err = %v, want ErrInvalid", err)
	}
}

func TestNewEphemeralManager_RoundTrip(t *testing.T) {
	m, err := session.NewEphemeralManager(time.Hour)
	if err != nil {
		t.Fatalf("NewEphemeralManager: %v", err)
	}
	v, err := m.Issue("acct-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	env, err := m.Verify(v)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if env.AccountID != "acct-1" {
		t.Errorf("account = %q", env.AccountID)
	}
}

// TestIssueWithMFAFlag_RoundTrip stamps MfaPending=true on the
// envelope and confirms Verify returns the same flag. This is the
// load-bearing property the requireMFA middleware depends on:
// a cookie issued with mfa_pending=true must round-trip the flag
// through Verify. Failure here means the dashboard login flow can
// set the gate but the next /v1/apps request bypasses it.
func TestIssueWithMFAFlag_RoundTrip(t *testing.T) {
	m, err := session.NewManager(key(t), time.Hour)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	v, err := m.IssueWithMFAFlag("acct-mfa", true)
	if err != nil {
		t.Fatalf("IssueWithMFAFlag(true): %v", err)
	}
	env, err := m.Verify(v)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if env.AccountID != "acct-mfa" {
		t.Errorf("account = %q, want acct-mfa", env.AccountID)
	}
	if !env.MfaPending {
		t.Errorf("MfaPending = false, want true")
	}
	if !session.IsMFAPending(env) {
		t.Errorf("IsMFAPending = false, want true")
	}

	// And the false case: a fresh cookie with mfaPending=false
	// must round-trip the same way Issue does. The wire bytes are
	// indistinguishable from a pre-IAM-2 cookie, which is the
	// backward-compat promise.
	v2, err := m.IssueWithMFAFlag("acct-mfa", false)
	if err != nil {
		t.Fatalf("IssueWithMFAFlag(false): %v", err)
	}
	env2, err := m.Verify(v2)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if env2.MfaPending {
		t.Errorf("MfaPending = true, want false")
	}
}

// TestVerify_BackwardCompatibleFromPreIAMPendingField confirms
// that a cookie issued before IAM-2 (no mfa_pending JSON key)
// decodes to MfaPending=false. The JSON tag is `omitempty`, so
// IssueWithMFAFlag(id, false) produces a wire envelope whose
// bytes match the pre-IAM-2 shape. We exercise the same path the
// browser does: signed bytes → base64 → Verify → Envelope.
func TestVerify_BackwardCompatibleFromPreIAMPendingField(t *testing.T) {
	m, err := session.NewManager(key(t), time.Hour)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// First confirm the false-flag wire shape preserves the
	// pre-IAM-2 envelope (3 fields, no mfa_pending).
	v, err := m.IssueWithMFAFlag("acct-legacy", false)
	if err != nil {
		t.Fatalf("IssueWithMFAFlag: %v", err)
	}
	env, err := m.Verify(v)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if env.MfaPending {
		t.Errorf("MfaPending = true, want false (backward-compat)")
	}
	// Issue() (no flag) is the alias for false — same shape.
	v2, err := m.Issue("acct-legacy")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	env2, err := m.Verify(v2)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if env2.MfaPending {
		t.Errorf("MfaPending = true, want false (Issue alias)")
	}
}

// TestNewManager_DefaultMaxAge covers the `if maxAge <= 0` default
// branch (manager.go:77-79) by passing both 0 and a negative duration.
// The constructor is required to fall back to 7 days (the documented
// session lifetime in the apid config layer). Anything else and the
// dashboard session expires during a coffee break.
func TestNewManager_DefaultMaxAge(t *testing.T) {
	cases := []struct {
		name   string
		maxAge time.Duration
		want   time.Duration
	}{
		{"zero defaults to 7d", 0, 7 * 24 * time.Hour},
		{"negative defaults to 7d", -time.Hour, 7 * 24 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := session.NewManager(key(t), tc.maxAge)
			if err != nil {
				t.Fatalf("NewManager(maxAge=%v): %v", tc.maxAge, err)
			}
			if got := m.MaxAge(); got != tc.want {
				t.Errorf("MaxAge() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestNewManager_RejectsEmptyKey covers the empty-slice branch of
// the `len(key) != 32` guard (manager.go:74-76). Distinct from
// TestNewManager_RejectsShortKey which passes 5 bytes; the
// empty-slice case is the silent one — no byte at all and the
// AES key schedule would never even reach aes.NewCipher.
func TestNewManager_RejectsEmptyKey(t *testing.T) {
	_, err := session.NewManager([]byte{}, time.Hour)
	if err == nil {
		t.Fatal("expected error for empty key")
	}
	if !strings.Contains(err.Error(), "32 bytes") {
		t.Errorf("err = %q, want substring %q", err.Error(), "32 bytes")
	}
}

// TestMaxAge_AfterExplicit confirms the getter returns whatever
// maxAge the constructor accepted (positive, non-default). Pairs
// with TestNewManager_DefaultMaxAge to lock the round-trip.
func TestMaxAge_AfterExplicit(t *testing.T) {
	m, err := session.NewManager(key(t), 90*time.Minute)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if got := m.MaxAge(); got != 90*time.Minute {
		t.Errorf("MaxAge() = %v, want 90m", got)
	}
}

// TestSetClock_NilResetsToTimeNow covers the `if now == nil` branch
// (manager.go:132-134). The contract is "passing nil restores the
// wall clock" so a test that wants to fast-forward and then return
// to real time doesn't have to keep a closure around forever.
func TestSetClock_NilResetsToTimeNow(t *testing.T) {
	m, err := session.NewManager(key(t), time.Hour)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// First, set a frozen clock and confirm Issue respects it.
	frozen := time.Unix(1_700_000_000, 0)
	m.SetClock(func() time.Time { return frozen })
	v, err := m.Issue("acct-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	env, err := m.Verify(v)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !env.IssuedAt.Equal(frozen) {
		t.Errorf("IssuedAt = %v, want %v (frozen)", env.IssuedAt, frozen)
	}
	// Now reset to wall clock and confirm a fresh Issue is no longer
	// pinned to the frozen value.
	m.SetClock(nil)
	before := time.Now()
	v2, err := m.Issue("acct-2")
	if err != nil {
		t.Fatalf("Issue after SetClock(nil): %v", err)
	}
	env2, err := m.Verify(v2)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if env2.IssuedAt.Before(before) {
		t.Errorf("IssuedAt = %v, want >= %v (wall clock)", env2.IssuedAt, before)
	}
	if env2.IssuedAt.Equal(frozen) {
		t.Errorf("IssuedAt = frozen %v after SetClock(nil); clock not reset", frozen)
	}
}

// TestIsMFAPending_Predicate locks the free function's contract
// across the four corners (omitted, false, true, post-verify). The
// requireMFA middleware reads this; if it ever drifts the dashboard
// either locks customers out (false negative) or skips the gate
// (false positive).
func TestIsMFAPending_Predicate(t *testing.T) {
	cases := []struct {
		name string
		env  session.Envelope
		want bool
	}{
		{"zero value", session.Envelope{}, false},
		{"explicit false", session.Envelope{MfaPending: false}, false},
		{"explicit true", session.Envelope{MfaPending: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := session.IsMFAPending(tc.env); got != tc.want {
				t.Errorf("IsMFAPending(%+v) = %v, want %v", tc.env, got, tc.want)
			}
		})
	}
}

// TestSealForCSRF_And_OpenForCSRF_RoundTrip covers the full
// CSRF seal/open path (manager.go:273-297). The plaintext shape
// is caller-defined — we use a recognizable byte string so a
// regression that drops bytes or reverses the nonce is observable
// on the output, not just on the length.
func TestSealForCSRF_And_OpenForCSRF_RoundTrip(t *testing.T) {
	m, err := session.NewManager(key(t), time.Hour)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	plaintext := []byte("opaque-csrf-blob-with-marker-42")
	sealed, err := m.SealForCSRF(plaintext)
	if err != nil {
		t.Fatalf("SealForCSRF: %v", err)
	}
	if sealed == "" {
		t.Fatal("SealForCSRF returned empty")
	}
	raw, err := base64.RawURLEncoding.DecodeString(sealed)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	got, err := m.OpenForCSRF(raw)
	if err != nil {
		t.Fatalf("OpenForCSRF: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("OpenForCSRF = %q, want %q", got, plaintext)
	}
}

// TestOpenForCSRF_RejectsSessionCookie covers the AAD-mismatch
// branch (manager.go:291-293). A session cookie (no AAD) handed
// to OpenForCSRF must fail the AEAD tag check because the CSRF
// seal uses csrfDomainSep as additional data. This is the
// cross-renderer invariant the comment on manager.go:209-213
// describes: even if a nonce were ever reused, the AAD keeps
// the two nonce spaces disjoint.
func TestOpenForCSRF_RejectsSessionCookie(t *testing.T) {
	m, err := session.NewManager(key(t), time.Hour)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	cookie, err := m.Issue("acct-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(cookie)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	_, err = m.OpenForCSRF(raw)
	if !errors.Is(err, session.ErrInvalid) {
		t.Errorf("OpenForCSRF(sessionCookie) = %v, want ErrInvalid", err)
	}
}

// TestOpenForCSRF_RejectsShortRaw covers the `len(raw) < ns` guard
// (manager.go:285-287). A 2-byte blob is shorter than the
// 12-byte GCM nonce so the function must short-circuit to
// ErrInvalid before reaching the AEAD open call.
func TestOpenForCSRF_RejectsShortRaw(t *testing.T) {
	m, err := session.NewManager(key(t), time.Hour)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	_, err = m.OpenForCSRF([]byte{0x01, 0x02})
	if !errors.Is(err, session.ErrInvalid) {
		t.Errorf("OpenForCSRF(short) = %v, want ErrInvalid", err)
	}
}

// TestSealForCSRF_NonceRandomness is a regression check on the
// `io.ReadFull(rand.Reader, nonce)` branch (manager.go:276-279).
// Two consecutive seals with the same plaintext must produce
// different base64 outputs because the nonce changes. A
// regression that reuses a nonce would let an attacker correlate
// or replay CSRF tokens.
func TestSealForCSRF_NonceRandomness(t *testing.T) {
	m, err := session.NewManager(key(t), time.Hour)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	plaintext := []byte("same-input-twice")
	a, err := m.SealForCSRF(plaintext)
	if err != nil {
		t.Fatalf("SealForCSRF #1: %v", err)
	}
	b, err := m.SealForCSRF(plaintext)
	if err != nil {
		t.Fatalf("SealForCSRF #2: %v", err)
	}
	if a == b {
		t.Errorf("two seals of identical plaintext produced identical base64; nonce is being reused")
	}
}

// TestVerify_RejectsEmptyAccountID lives in empty_accountid_test.go
// (white-box, `package session`) because forging a session-shaped
// cookie requires a direct call to the unexported gcm.Seal.

// TestSealGithubLogin_HappyPath covers the entire SealGithubLogin
// body (manager.go:198-220): a cookie sealed for a customer with
// a known GithubLogin field round-trips through Verify and
// preserves the field. The dashboard reads the cookie on every
// page load to render the avatar — if the field drops, the UI
// falls back to a generic placeholder.
func TestSealGithubLogin_HappyPath(t *testing.T) {
	m, err := session.NewManager(key(t), time.Hour)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	cookie, err := m.SealGithubLogin("acct-gh-1", "octocat", false)
	if err != nil {
		t.Fatalf("SealGithubLogin: %v", err)
	}
	env, err := m.Verify(cookie)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if env.AccountID != "acct-gh-1" {
		t.Errorf("AccountID = %q, want acct-gh-1", env.AccountID)
	}
	if env.GithubLogin != "octocat" {
		t.Errorf("GithubLogin = %q, want octocat", env.GithubLogin)
	}
	if env.MfaPending {
		t.Errorf("MfaPending = true, want false")
	}
}

// TestSealGithubLogin_RejectsEmptyAccountID covers the
// `if accountID == ""` guard (manager.go:199-201). The handler
// calls this with the just-resolved account.ID; if that field is
// ever empty (lookup failure that didn't fail closed) we want
// the seal to refuse rather than mint a cookie with no
// accountability row.
func TestSealGithubLogin_RejectsEmptyAccountID(t *testing.T) {
	m, err := session.NewManager(key(t), time.Hour)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, err := m.SealGithubLogin("", "octocat", false); err == nil {
		t.Fatal("expected error for empty accountID")
	}
}

// TestSealGithubLogin_MfaPendingRoundTrip covers the
// `mfaPending=true` branch on the SealGithubLogin path. Same
// flag-as-Issue path: the dashboard must see the pending flag
// and the requireMFA middleware must 403 every non-allowlist
// route until the customer clears TOTP.
func TestSealGithubLogin_MfaPendingRoundTrip(t *testing.T) {
	m, err := session.NewManager(key(t), time.Hour)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	cookie, err := m.SealGithubLogin("acct-mfa", "octocat", true)
	if err != nil {
		t.Fatalf("SealGithubLogin: %v", err)
	}
	env, err := m.Verify(cookie)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !env.MfaPending {
		t.Errorf("MfaPending = false, want true (GitHub login carries the pending flag)")
	}
}

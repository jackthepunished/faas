package mail

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestResendSignature_RoundTrip pins the happy path: a payload
// signed with SignResendForTest verifies successfully against
// VerifyResendSignature.
func TestResendSignature_RoundTrip(t *testing.T) {
	secret := RandomResendSecretForTest()
	body := []byte(`{"type":"email.bounced","data":{"email":"a@b"}}`)
	id := "msg_test_123"
	ts := formatUnix(time.Now())
	sig := SignResendForTest(body, secret, id, ts)
	if err := VerifyResendSignature(body, sig, secret, id, ts, time.Minute); err != nil {
		t.Fatalf("round-trip failed: %v", err)
	}
}

// TestResendSignature_RejectsBadSignature pins that a tampered
// signature returns ErrBadSignature.
func TestResendSignature_RejectsBadSignature(t *testing.T) {
	secret := RandomResendSecretForTest()
	body := []byte(`{"type":"email.bounced"}`)
	id := "msg_test_456"
	ts := formatUnix(time.Now())
	badSig := "v1," + strings.Repeat("A", 44) // base64 length 32 bytes
	if err := VerifyResendSignature(body, badSig, secret, id, ts, time.Minute); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("bad signature: err = %v, want ErrBadSignature", err)
	}
}

// TestResendSignature_RejectsTamperedBody pins that modifying
// the body invalidates the signature even when the signature
// itself is well-formed.
func TestResendSignature_RejectsTamperedBody(t *testing.T) {
	secret := RandomResendSecretForTest()
	body := []byte(`{"type":"email.bounced","email":"a@b"}`)
	id := "msg_test_789"
	ts := formatUnix(time.Now())
	sig := SignResendForTest(body, secret, id, ts)
	tampered := []byte(`{"type":"email.bounced","email":"attacker@evil"}`)
	if err := VerifyResendSignature(tampered, sig, secret, id, ts, time.Minute); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("tampered body: err = %v, want ErrBadSignature", err)
	}
}

// TestResendSignature_RejectsStaleTimestamp pins that the
// timestamp tolerance window is enforced. A 4-min-old delivery
// is fine; a 10-min-old delivery is rejected.
func TestResendSignature_RejectsStaleTimestamp(t *testing.T) {
	secret := RandomResendSecretForTest()
	body := []byte(`{}`)
	id := "msg_test_stale"
	// Use the actual current time minus the threshold so the
	// verifier's tolerance calc lands at the boundary.
	now := time.Now().UTC()
	for _, offset := range []time.Duration{-10 * time.Minute, 10 * time.Minute} {
		tsStr := formatUnix(now.Add(offset))
		sig := SignResendForTest(body, secret, id, tsStr)
		err := VerifyResendSignature(body, sig, secret, id, tsStr, 5*time.Minute)
		if !errors.Is(err, ErrBadSignature) {
			t.Fatalf("stale ts (offset=%s): err = %v, want ErrBadSignature", offset, err)
		}
	}
	// 1-min-old delivery: must verify (within tolerance).
	tsStr := formatUnix(now.Add(-1 * time.Minute))
	sig := SignResendForTest(body, secret, id, tsStr)
	if err := VerifyResendSignature(body, sig, secret, id, tsStr, 5*time.Minute); err != nil {
		t.Fatalf("1-min-old delivery should verify: %v", err)
	}
}

// TestResendSignature_RejectsEmptySecret pins the fail-closed
// path: an empty secret returns ErrBadSignature rather than
// verifying everything.
func TestResendSignature_RejectsEmptySecret(t *testing.T) {
	body := []byte(`{}`)
	err := VerifyResendSignature(body, "v1,xxxx", "", "msg_1", "1700000000", time.Minute)
	if !errors.Is(err, ErrBadSignature) {
		t.Fatalf("empty secret: err = %v, want ErrBadSignature", err)
	}
}

// TestResendSignature_RejectsMissingPrefix pins that a secret
// without the whsec_ prefix is rejected. Resend always sends
// the prefix; a prefix-less secret is a misconfiguration that
// must fail loud.
func TestResendSignature_RejectsMissingPrefix(t *testing.T) {
	body := []byte(`{}`)
	err := VerifyResendSignature(body, "v1,xxxx", "notprefixed", "msg_1", "1700000000", time.Minute)
	if !errors.Is(err, ErrBadSignature) {
		t.Fatalf("missing prefix: err = %v, want ErrBadSignature", err)
	}
}

// TestResendSignature_AcceptsMultiVersionHeader pins that
// `svix-signature: v1,<sig> v1,<sig2>` works — Svix reserves
// the right to add new versions and the verifier must keep
// v1 working through the migration.
func TestResendSignature_AcceptsMultiVersionHeader(t *testing.T) {
	secret := RandomResendSecretForTest()
	body := []byte(`{}`)
	id := "msg_multi"
	ts := formatUnix(time.Now())
	sig := SignResendForTest(body, secret, id, ts)
	// Header with the real signature followed by a junk
	// version the verifier must ignore.
	header := sig + " v2," + strings.Repeat("B", 44)
	if err := VerifyResendSignature(body, header, secret, id, ts, time.Minute); err != nil {
		t.Fatalf("multi-version header should still verify: %v", err)
	}
}

// TestResendSignature_RejectsMissingHeaders pins the input
// validation surface: empty signature / id / timestamp headers
// must fail loudly so the apid handler returns 400.
func TestResendSignature_RejectsMissingHeaders(t *testing.T) {
	secret := RandomResendSecretForTest()
	body := []byte(`{}`)
	now := formatUnix(time.Now())
	sig := SignResendForTest(body, secret, "msg_x", now)
	cases := []struct {
		name string
		sigH string
		idH  string
		tsH  string
	}{
		{"missing signature", "", "msg_x", now},
		{"missing id", sig, "", now},
		{"missing timestamp", sig, "msg_x", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := VerifyResendSignature(body, tc.sigH, secret, tc.idH, tc.tsH, time.Minute)
			if !errors.Is(err, ErrBadSignature) {
				t.Fatalf("%s: err = %v, want ErrBadSignature", tc.name, err)
			}
		})
	}
}

// formatUnix is a tiny helper that mirrors strconv.FormatInt's
// unix-seconds representation. Kept inline so the stale-timestamp
// test doesn't pull strconv into the call site.
func formatUnix(t time.Time) string {
	return strconv.FormatInt(t.Unix(), 10)
}

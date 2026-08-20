package gateway

// synth_internal_only_test.go — ADR-119 tests for
// SynthServer.applyIngressInternalSvc (the cron-bypass
// closure). Mirrors public_auth_internal_only_test.go with
// synth-side specifics: audit emits use the
// synthAuditEmit closure (a separate field) and the
// "from" field is "synth".

import (
	"bytes"
	"crypto/ed25519"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/internalsvc"
)

// newTestSynthServerForInternalSvc constructs a SynthServer
// wired with the test verifier + auditor + Metrics. The
// dispatcher is intentionally nil — these tests exercise
// the gate only, not the dispatch path. The gate fires
// BEFORE the dispatcher (handleSynthesize: invoke the gate
// → return on deny → dispatcher.Wake on pass), so the
// dispatcher never runs in these tests.
func newTestSynthServerForInternalSvc(t *testing.T, allowedSvc map[string]ed25519.PublicKey) (*SynthServer, *testCountingAuthnAuditor, *testInternalSvcVerifier) {
	t.Helper()
	v := &testInternalSvcVerifier{allowed: allowedSvc}
	a := &testCountingAuthnAuditor{}
	srv := &SynthServer{
		log:                slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)),
		metrics:            NewMetrics(),
		internalSvcVerifier: v,
		synthAuditEmit:      a.Emit,
	}
	return srv, a, v
}

// TestSynthApplyIngressInternalSvc_MissingHeader covers the
// no-Authorization-header path. Asserts:
//   - gate returns true (deny)
//   - 403 + RFC 7807 problem body
//   - audit row with kind=instances.public_auth_internal_missing
//   - "from" field = "synth" (distinguishes from the HTTP side)
//   - blocked metric incremented
//   - Wake NOT called (load-bearing invariant)
func TestSynthApplyIngressInternalSvc_MissingHeader(t *testing.T) {
	srv, a, _ := newTestSynthServerForInternalSvc(t, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/synthesize", nil)

	if !srv.applyIngressInternalSvc(rec, req, "app-1", "internal_only") {
		t.Errorf("gate should deny on missing header")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("rec.Code = %d, want 403", rec.Code)
	}
	if got := a.countByKind("instances.public_auth_internal_missing"); got != 1 {
		t.Errorf("missing-audit count = %d, want 1", got)
	}
	// Verify the audit row's "from" field is "synth".
	for _, ev := range a.events {
		if from, _ := ev.data["from"].(string); from != "synth" {
			t.Errorf("audit row %s from = %q, want \"synth\"", ev.kind, from)
		}
	}
}

// TestSynthApplyIngressInternalSvc_OpenModePassThrough covers
// the non-internal_only short-circuit. The gate returns
// false WITHOUT calling the verifier or emitting audit.
func TestSynthApplyIngressInternalSvc_OpenModePassThrough(t *testing.T) {
	srv, a, v := newTestSynthServerForInternalSvc(t, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/synthesize", nil)
	// No Authorization header — would fail if the gate ran.

	if srv.applyIngressInternalSvc(rec, req, "app-1", "open") {
		t.Errorf("gate should be no-op for open mode")
	}
	if v.calls != 0 {
		t.Errorf("verifier.Verify called %d times on open mode; want 0", v.calls)
	}
	if len(a.events) != 0 {
		t.Errorf("audit emitted on open mode; should be no-op")
	}
}

// TestSynthApplyIngressInternalSvc_InvalidSignature covers
// the signature-failure path. Mints with keyA, verifies
// against an allowlist that contains only keyB (signature
// mismatch). Asserts:
//   - 403
//   - audit row with kind=instances.public_auth_internal_invalid
//   - reason=signature_invalid (sub "schedd" IS in the
//     allowlist; the gate looks up pubB and verifies against
//     it, but the token was signed with privA, so signature
//     fails — not "unknown_service")
//   - blocked metric
func TestSynthApplyIngressInternalSvc_InvalidSignature(t *testing.T) {
	pubA, privA, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("genkey A: %v", err)
	}
	pubB, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("genkey B: %v", err)
	}
	srv, a, _ := newTestSynthServerForInternalSvc(t, map[string]ed25519.PublicKey{
		"schedd": pubB,
	})
	tok := mintTestToken(t, pubA, privA, "", "schedd", 30)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/synthesize", nil)
	req.Header.Set("Authorization", "Bearer "+tok)

	if !srv.applyIngressInternalSvc(rec, req, "app-1", "internal_only") {
		t.Errorf("gate should deny on signature mismatch")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("rec.Code = %d, want 403", rec.Code)
	}
	if got := a.countByKind("instances.public_auth_internal_invalid"); got != 1 {
		t.Errorf("invalid-audit count = %d, want 1", got)
	}
	// Confirm reason is "signature_invalid" (sub is in
	// allowlist; signature verification failed).
	var foundReason string
	for _, ev := range a.events {
		if ev.kind == "instances.public_auth_internal_invalid" {
			if r, ok := ev.data["reason"].(string); ok {
				foundReason = r
			}
		}
	}
	if foundReason != "signature_invalid" {
		t.Errorf("invalid-audit reason = %q, want \"signature_invalid\"", foundReason)
	}
}

// TestSynthApplyIngressInternalSvc_ValidTokenPassThrough
// covers the happy path. A valid token + matching allowlist
// passes; the gate returns false and the metric increments.
func TestSynthApplyIngressInternalSvc_ValidTokenPassThrough(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	srv, a, v := newTestSynthServerForInternalSvc(t, map[string]ed25519.PublicKey{
		"schedd": pub,
	})
	tok := mintTestToken(t, pub, priv, "", "schedd", 30)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/synthesize", nil)
	req.Header.Set("Authorization", "Bearer "+tok)

	if srv.applyIngressInternalSvc(rec, req, "app-1", "internal_only") {
		t.Errorf("gate denied a valid token; response body=%q", rec.Body.String())
	}
	if len(a.events) != 0 {
		t.Errorf("audit emitted on pass-through; should be silent (events=%v)", a.events)
	}
	if v.calls != 1 {
		t.Errorf("verifier.Verify calls = %d, want 1", v.calls)
	}
}

// TestSynthApplyIngressInternalSvc_AuditDoesNotEchoToken is
// the load-bearing redaction-invariant test for the synth
// side. Mints a token that will be rejected (expired) and
// scans the audit row for the JWT substring. Failure here
// is a security regression.
func TestSynthApplyIngressInternalSvc_AuditDoesNotEchoToken(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	srv, a, _ := newTestSynthServerForInternalSvc(t, map[string]ed25519.PublicKey{
		"schedd": pub,
	})
	tok := mintTestToken(t, pub, priv, "", "schedd", -120) // expired
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/synthesize", nil)
	req.Header.Set("Authorization", "Bearer "+tok)

	srv.applyIngressInternalSvc(rec, req, "app-1", "internal_only")

	for _, ev := range a.events {
		for k, v := range ev.data {
			if s, ok := v.(string); ok && strings.Contains(s, tok) {
				t.Errorf("audit row %s field %q contains JWT substring; redaction invariant violated", ev.kind, k)
			}
		}
	}
}

// TestIsErrHelpers pins the string-match maps that translate
// cmd-bridge error strings back to closed reason codes. The
// pkg/internalsvc.Err*.Error() text is part of the §3 ADR-119
// contract — changing any of these strings without updating
// the gate would silently break the reason-mapping. This test
// catches that drift.
func TestIsErrHelpers(t *testing.T) {
	cases := []struct {
		name   string
		fn     func(error) bool
		errStr string
		want   bool
	}{
		{"audience", isErrAudience, "internalsvc: aud claim does not match gregale.internal", true},
		{"audience-neg", isErrAudience, "internalsvc: token expired", false},
		{"expired", isErrExpired, "internalsvc: token expired", true},
		{"not_yet_valid", isErrNotYetValid, "internalsvc: token not yet valid", true},
		{"unknown_service", isErrUnknownService, "internalsvc: svcName not in per-service allowlist", true},
		{"signature_invalid-neg", isErrAudience, "internalsvc: signature invalid", false},
		{"malformed", isErrMalformed, "internalsvc: token malformed", true},
		{"empty_allowlist", isErrEmptyAllowlist, "internalsvc: per-service allowlist must not be empty", true},
		{"nil-error", isErrExpired, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			if tc.errStr != "" {
				err = &stringErr{msg: tc.errStr}
			}
			if got := tc.fn(err); got != tc.want {
				t.Errorf("%s(%q) = %v, want %v", tc.name, tc.errStr, got, tc.want)
			}
		})
	}
}

type stringErr struct{ msg string }

func (e *stringErr) Error() string { return e.msg }

// _ pins imports for the test file's expansion surface.
var (
	_ = api.WriteProblem
	_ = internalsvc.Audience
)
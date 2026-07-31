// Tests for VerifyCNClosure, AllowAllNodeVerifier, and the
// ErrNodeVerifierCNMismatch wrapping contract (ADR-056).
//
// These tests cover the public surface of node_verifier.go. The
// implementation-specific tests live in:
//   - inmemverifier_test.go (InmemNodeVerifier)
//   - pgverifier_test.go (PGNodeVerifier)
//
// Coverage:
//   - VerifyCNClosure returns nil when v is nil (canonical AllowAll).
//   - VerifyCNClosure errors when verifiedChains is empty or empty
//     leaf (defensive guard against future refactors).
//   - VerifyCNClosure errors when the leaf CN is empty.
//   - VerifyCNClosure calls v.LookupCN(leaf.Subject.CommonName) and
//     propagates its result.
//   - AllowAllNodeVerifier.LookupCN returns nil for every CN, and
//     nil-receiver also returns nil.

package wire

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"testing"
)

func TestVerifyCNClosure_NilVerifierReturnsNilHook(t *testing.T) {
	hook := VerifyCNClosure(nil)
	if hook != nil {
		t.Errorf("VerifyCNClosure(nil) returned non-nil hook; want nil (canonical AllowAll)")
	}
}

func TestVerifyCNClosure_EmptyVerifiedChainsErrors(t *testing.T) {
	v := NewInmemNodeVerifier()
	v.Set([]string{"vmmd.faas"})
	hook := VerifyCNClosure(v)
	if hook == nil {
		t.Fatal("hook unexpectedly nil")
	}

	// Empty chains → defensive error (stdlib should never have
	// invoked us, but the guard keeps the verifier from silently
	// no-op'ing).
	if err := hook(nil, nil); err == nil {
		t.Errorf("hook with nil chains returned nil err; want non-nil")
	}
	if err := hook(nil, [][]*x509.Certificate{}); err == nil {
		t.Errorf("hook with empty chains returned nil err; want non-nil")
	}
	if err := hook(nil, [][]*x509.Certificate{{}}); err == nil {
		t.Errorf("hook with empty leaf chain returned nil err; want non-nil")
	}
}

func TestVerifyCNClosure_EmptyCNOnLeafErrors(t *testing.T) {
	v := NewInmemNodeVerifier()
	v.Set([]string{"vmmd.faas"})
	hook := VerifyCNClosure(v)

	leaf := &x509.Certificate{
		Subject: pkix.Name{CommonName: ""}, // empty CN
	}
	err := hook(nil, [][]*x509.Certificate{{leaf}})
	if err == nil {
		t.Fatalf("hook with empty CN leaf returned nil err; want non-nil")
	}
	// The empty-CN guard is a tamper signal (legitimate leaves
	// carry a CN). It is a separate error path from
	// ErrNodeVerifierCNMismatch (the latter fires when the leaf's
	// CN is registered-but-not-allowed). Asserting non-nil is the
	// correct contract.
}

func TestVerifyCNClosure_AcceptsRegisteredCN(t *testing.T) {
	v := NewInmemNodeVerifier()
	v.Set([]string{"vmmd.faas"})
	hook := VerifyCNClosure(v)

	leaf := &x509.Certificate{
		Subject: pkix.Name{CommonName: "vmmd.faas"},
	}
	if err := hook(nil, [][]*x509.Certificate{{leaf}}); err != nil {
		t.Errorf("hook with registered CN=%v; want nil", err)
	}
}

func TestVerifyCNClosure_RejectsUnregisteredCN(t *testing.T) {
	v := NewInmemNodeVerifier()
	v.Set([]string{"vmmd.faas"})
	hook := VerifyCNClosure(v)

	leaf := &x509.Certificate{
		Subject: pkix.Name{CommonName: "schedd.faas"},
	}
	err := hook(nil, [][]*x509.Certificate{{leaf}})
	if err == nil {
		t.Fatalf("hook with unregistered CN returned nil err; want non-nil")
	}
	if !errors.Is(err, ErrNodeVerifierCNMismatch) {
		t.Errorf("err=%v; want wraps ErrNodeVerifierCNMismatch", err)
	}
}

func TestAllowAllNodeVerifier_NeverRejects(t *testing.T) {
	var v *AllowAllNodeVerifier
	for _, cn := range []string{"", "anything", "vmmd.faas"} {
		if err := v.LookupCN(cn); err != nil {
			t.Errorf("AllowAllNodeVerifier.LookupCN(%q)=%v; want nil", cn, err)
		}
	}
}

func TestAllowAllNodeVerifier_NilReceiverIsNilSafe(t *testing.T) {
	var v *AllowAllNodeVerifier
	if err := v.LookupCN("anything"); err != nil {
		t.Errorf("nil AllowAllNodeVerifier.LookupCN()=%v; want nil", err)
	}
}

func TestNodeVerifierWithCN_NilErrorReturnsNil(t *testing.T) {
	if err := nodeVerifierWithCN(nil, "x"); err != nil {
		t.Errorf("nodeVerifierWithCN(nil, _)=%v; want nil", err)
	}
}

func TestNodeVerifierWithCN_WrapsCN(t *testing.T) {
	err := nodeVerifierWithCN(ErrNodeVerifierCNMismatch, "vmmd.faas")
	if !errors.Is(err, ErrNodeVerifierCNMismatch) {
		t.Errorf("err=%v; want wraps ErrNodeVerifierCNMismatch", err)
	}
}

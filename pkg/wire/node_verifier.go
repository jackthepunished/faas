// Handshake-layer leaf-CN binding for the multi-box wire (ADR-056).
//
// ADR-052 §Handler-layer peer binding covers per-handler role checks via
// wire.PeerCN(ctx). This file adds a SECOND layer — invoked by
// crypto/tls itself after the stdlib chain/SAN/EKU check passes —
// that rejects any peer whose leaf-CN is not present in a registered
// set (production: a snapshot of compute_nodes.name; tests: an
// in-memory CN set).
//
// The composition with stdlib is the load-bearing detail:
//
//   - crypto/tls runs chain trust against RootCAs / ClientCAs, RFC
//     6125 SAN matching, and EKU enforcement BEFORE the
//     VerifyPeerCertificate hook is consulted. If any of those
//     fail, the handshake aborts and the hook is never invoked.
//     Our verifier therefore can NEVER see an untrusted leaf.
//   - InsecureSkipVerify stays false. ADR-052 §Rejected alternatives
//     flagged InsecureSkipVerify=true + a custom verifier as
//     CodeQL alert #58; this file does NOT touch that field.
//
// The interface is intentionally narrow — single method LookupCN —
// because every implementation (production PG, test in-memory,
// default allow-all) maps cleanly onto "is this CN registered?".

package wire

import (
	"crypto/x509"
	"errors"
	"fmt"
)

// ErrNodeVerifierCNMismatch is returned when the leaf-CN is not in
// the verifier's registered set. The closure built by VerifyCNClosure
// surfaces it to crypto/tls as a handshake failure; gRPC maps that
// to codes.Unauthenticated on the dial side and a transport error
// on the listener side.
var ErrNodeVerifierCNMismatch = errors.New("wire: peer CN not in compute_node registry")

// ErrNilNodeVerifierPool is returned by daemon-side wiring when
// runNodeVerifier is called with a nil pool. Reaching this branch
// is a programming error (the caller is supposed to gate on
// cfg.ComputeNode.NodeName != ""), so the error is loud and
// distinguishable from load-time failures.
var ErrNilNodeVerifierPool = errors.New("wire: node verifier started with nil pool")

// NodeVerifier is the handshake-layer CN-binding seam. The stdlib TLS
// verifier (chain/SAN/EKU) runs first in the same pass; this hook is
// invoked AFTER stdlib trust succeeds, so the verifier augments
// (never replaces) stdlib's chain check.
//
// The verifier is a thin LookupCN: production implementations resolve
// against an in-memory snapshot of compute_nodes.name (the friendly
// label that matches Subject.CommonName). The snapshot is rebuilt
// from Postgres on every 'compute_node_changed' pg_notify tick — see
// PGNodeVerifier.
//
// The verifier MUST be safe for concurrent use: gRPC's tlsCreds
// ServerHandshake invokes the hook on a single goroutine per
// connection, but multiple connections invoke it in parallel.
//
// A nil receiver is treated as AllowAll — guards any future caller
// that forgets to wire the verifier on the prod path.
type NodeVerifier interface {
	// LookupCN returns nil when the given CN is registered.
	// Any non-nil error causes the handshake to abort.
	LookupCN(cn string) error
}

// VerifyCNClosure builds a tls.Config.VerifyPeerCertificate callback
// that consults v.LookupCN on the verified leaf certificate. The
// callback is invoked by crypto/tls AFTER the stdlib verifier has
// confirmed chain trust + SAN + EKU, so the function expects
// verifiedChains[0][0] to be a fully-trusted leaf cert.
//
// The closure is the canonical installation point: callers store the
// returned func in tls.Config.VerifyPeerCertificate via the
// Load*TLSConfigWithVerifier factory variants. Returning nil is
// permitted (= AllowAll — the verifier is empty or disabled).
//
// On any non-nil LookupCN error, the closure returns that error so
// crypto/tls surfaces it as a TLS handshake failure; gRPC maps that
// to codes.Unauthenticated.
func VerifyCNClosure(v NodeVerifier) func([][]byte, [][]*x509.Certificate) error {
	if v == nil {
		return nil
	}
	return func(_ [][]byte, verifiedChains [][]*x509.Certificate) error {
		if len(verifiedChains) == 0 || len(verifiedChains[0]) == 0 {
			// stdlib didn't trust the leaf — this shouldn't happen
			// because stdlib runs first, but a defensive guard keeps
			// a future pkg/wire refactor from silently no-op-ing.
			return errors.New("wire: verifier invoked without verified chain")
		}
		leaf := verifiedChains[0][0]
		cn := leaf.Subject.CommonName
		if cn == "" {
			// Mirrors wire.PeerCN's ErrPeerCNUnavailable contract:
			// empty CN on a TLS-handshake peer is itself a tamper
			// signal (legitimate per-daemon leaves carry a CN).
			return errors.New("wire: verifier invoked with empty CN on leaf")
		}
		return v.LookupCN(cn)
	}
}

// AllowAllNodeVerifier is the no-op default. Used by single-box dev
// installs and pre-slice-3 paths where the registry is irrelevant
// (no TLS on unix sockets in the legacy single-box path; or no
// compute_nodes table on legacy schedd).
//
// A nil *AllowAllNodeVerifier also returns nil (nil-safe) so the
// factory variants can wire an unconfigured verifier without
// special-casing.
type AllowAllNodeVerifier struct{}

// LookupCN returns nil unconditionally — every CN is "registered".
func (*AllowAllNodeVerifier) LookupCN(string) error { return nil }

// Ensure AllowAllNodeVerifier satisfies NodeVerifier at compile time.
// A forgotten method would fail here, not at the first dial.
var _ NodeVerifier = (*AllowAllNodeVerifier)(nil)

// nodeVerifierWithCN wraps an error with the rejected CN for
// diagnostics. Wrapping is via %w so errors.Is matches
// ErrNodeVerifierCNMismatch across calls.
func nodeVerifierWithCN(err error, cn string) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: cn=%q", err, cn)
}

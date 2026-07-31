// Peer CN extraction — handler-layer identity binding (ADR-052 §Handler-
// layer peer binding). The stdlib verifier (chain trust, RFC 6125 SAN
// matching, EKU enforcement) runs in a single handshake pass and is
// the load-bearing trust gate; this helper is the SECOND tier, applied
// at the handler entry point. It pulls the peer's certificate CN out
// of the gRPC peer context and returns it so callers can compare
// against an expected role (e.g. "schedd.faas" for a vmmd→schedd RPC,
// "vmmd.faas" for a schedd→vmmd RPC).
//
// The CN-vs-SAN question: RFC 6125 (and Go's verifier) treat SANs as
// authoritative when present; the CN is informational. The per-daemon
// leaf we issue under pkg/pki carries BOTH the per-daemon SAN
// (ProductionSANs(cn)) and the CN=<daemon>.faas string, so callers
// can use either side — CN for the friendly role lookup, SAN for the
// crypto verifier. Today we use CN at the handler layer because it
// reads better in audit logs and is the canonical role identity in
// pkg/pki.Roles(). The stdlib verifier (which is what actually
// decides trust) does not care which field the handler reads.
//
// Failure mode: PeerCN returns an error when the peer didn't arrive
// over mTLS, when the credentials aren't TLS, or when the leaf has
// no CN. Handlers should map that to codes.Unauthenticated — refusing
// the call rather than silently allowing an anonymous peer.

package wire

import (
	"context"
	"errors"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

// ErrPeerCNUnavailable is returned when the context has no gRPC peer
// metadata (single-box default-local path, or a unix-socket dial
// without TLS). Handlers should refuse such calls when the method
// requires mTLS; callers can ignore the error when the method is
// intentionally permissive (e.g. an internal healthz RPC).
var ErrPeerCNUnavailable = errors.New("wire: peer CN unavailable (no TLS on the connection)")

// PeerCN returns the Common Name from the peer's leaf certificate,
// or ErrPeerCNUnavailable when no TLS credentials are on the
// connection. The context must carry a gRPC peer.Peer — every
// handler receives one in production; unit tests that bypass gRPC
// must construct one via peer.NewContext.
func PeerCN(ctx context.Context) (string, error) {
	p, ok := peer.FromContext(ctx)
	if !ok || p == nil {
		return "", ErrPeerCNUnavailable
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return "", ErrPeerCNUnavailable
	}
	if len(tlsInfo.State.VerifiedChains) == 0 || len(tlsInfo.State.VerifiedChains[0]) == 0 {
		return "", ErrPeerCNUnavailable
	}
	cn := tlsInfo.State.VerifiedChains[0][0].Subject.CommonName
	if cn == "" {
		return "", ErrPeerCNUnavailable
	}
	return cn, nil
}

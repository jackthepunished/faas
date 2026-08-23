// peer_extra_test.go — fill the remaining 8.3% of PeerCN coverage:
// the "TLSInfo present + chains present + leaf CN is empty" branch
// at peer.go:60-62. The existing peer_test.go covers the no-peer,
// no-TLS, and empty-chain branches but does not pin the empty-CN
// short-circuit. A regression that silently returned the empty
// string would let an attacker impersonate "any daemon" by minting
// a leaf cert with Subject.CommonName="".

package wire

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"net"
	"testing"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

// TestPeerCN_EmptyLeafCNReturnsUnavailable pins the "verified
// chain present but leaf CN is the empty string" branch at
// peer.go:60-62. The leaf certificate subject is the empty struct,
// which has Subject.CommonName == "". PeerCN must refuse this —
// callers depend on the CN being a non-empty role identity.
func TestPeerCN_EmptyLeafCNReturnsUnavailable(t *testing.T) {
	leaf := &x509.Certificate{Subject: pkix.Name{}} // empty CN
	ctx := peer.NewContext(context.Background(), &peer.Peer{
		Addr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1},
		AuthInfo: credentials.TLSInfo{
			State: tls.ConnectionState{
				VerifiedChains: [][]*x509.Certificate{{leaf}},
			},
		},
	})
	if _, err := PeerCN(ctx); err == nil {
		t.Fatal("PeerCN(empty CN) = nil, want ErrPeerCNUnavailable")
	} else if !errors.Is(err, ErrPeerCNUnavailable) {
		t.Fatalf("PeerCN(empty CN) = %v, want ErrPeerCNUnavailable", err)
	}
}

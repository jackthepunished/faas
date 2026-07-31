// PeerCN tests for pkg/wire/peer.go. Pairs the stdlib verifier's
// chain-trust gate (chain/SAN/EKU) with the handler-layer CN
// extraction that ADR-052 §Handler-layer peer binding adds as the
// second tier — together they form the load-bearing identity check
// for every remote gRPC call in the multi-box control plane.

package wire

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/peer"
)

// TestPeerCN_AvailableOverMTLS dials the existing health.Check RPC
// over an mTLS listener and asserts that the peer CN is reachable
// inside the unary interceptor. The CN is "test-server" (from
// newTestPKI), so the assertion proves end-to-end that
// peer.FromContext + credentials.TLSInfo round-trip works through
// grpc.NewServer + grpc.NewClient.
//
// We use a unary interceptor (rather than a custom service
// descriptor) because the interceptor sees the same peer.FromContext
// a real handler would see, with no risk of gRPC's HandlerType/
// reflection-free service registration rejecting our test impl
// across gRPC versions. The interceptor captures PeerCN into a
// closure variable the test reads after the RPC returns.
func TestPeerCN_AvailableOverMTLS(t *testing.T) {
	pki := newTestPKI(t)
	serverTLS, err := LoadServerTLSConfig(pki.serverCert, pki.serverKey, pki.caCert)
	if err != nil {
		t.Fatalf("LoadServerTLSConfig: %v", err)
	}
	clientTLS, err := LoadClientTLSConfig(pki.clientCert, pki.clientKey, pki.caCert)
	if err != nil {
		t.Fatalf("LoadClientTLSConfig: %v", err)
	}

	lis, err := Listen(context.Background(), "tcp://127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = lis.Close() })

	// Capture the peer's CN via a unary interceptor. The interceptor
	// runs at the same point a real handler would — after gRPC has
	// populated peer.FromContext — so reading PeerCN here is the
	// exact contract ADR-052 §Handler-layer peer binding relies on.
	var capturedCN string
	var sawTLSInfo bool
	interceptor := func(ctx context.Context, req interface{}, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if p, ok := peer.FromContext(ctx); ok {
			// Surface the actual AuthInfo type for debug when the
			// test fails — the type name tells us whether gRPC
			// wrapped TLSInfo (e.g. via a credentials.ProtocolInfo)
			// or whether it's bare.
			t.Logf("peer.AuthInfo type = %T", p.AuthInfo)
			if _, ok := p.AuthInfo.(credentials.TLSInfo); ok {
				sawTLSInfo = true
			}
		}
		cn, perr := PeerCN(ctx)
		if perr == nil {
			capturedCN = cn
		}
		return handler(ctx, req)
	}

	opts := []grpc.ServerOption{grpc.UnaryInterceptor(interceptor)}
	opts = append(opts, ServerCredsOrEmpty(serverTLS)...)
	srv := grpc.NewServer(opts...)
	hs := newCapturingHealthServer()
	healthgrpc.RegisterHealthServer(srv, hs)
	hs.setServing()
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { srv.Stop() })

	conn, err := Dial(context.Background(), "tcp://"+lis.Addr().String(), clientTLS)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := healthgrpc.NewHealthClient(conn).Check(ctx, &healthgrpc.HealthCheckRequest{}); err != nil {
		t.Fatalf("Health/Check: %v", err)
	}
	if !sawTLSInfo {
		t.Fatalf("peer.AuthInfo did not carry credentials.TLSInfo (gRPC server transport should populate it on every TLS connection)")
	}
	if capturedCN != "test-client" {
		t.Fatalf("PeerCN = %q, want %q (server sees the client's CN via the verified chain)", capturedCN, "test-client")
	}
}

// TestPeerCN_UnitOnContextWithoutPeer proves the helper refuses
// contexts that don't carry a gRPC peer — the e2e test harness and
// the daemon's startup wiring both rely on this fail-closed
// behaviour to catch accidental TLS-stripping regressions.
func TestPeerCN_UnitOnContextWithoutPeer(t *testing.T) {
	_, err := PeerCN(context.Background())
	if !errors.Is(err, ErrPeerCNUnavailable) {
		t.Fatalf("PeerCN(context.Background()) = %v, want ErrPeerCNUnavailable", err)
	}
}

// TestPeerCN_UnitOnNonTLSPeer proves the helper also refuses a peer
// whose AuthInfo is NOT TLSInfo (e.g. a unix-socket dial). This
// pins the second-tier check: every TLS-stripped RPC must surface
// as ErrPeerCNUnavailable, never a silent empty string.
func TestPeerCN_UnitOnNonTLSPeer(t *testing.T) {
	// A peer.Peer with no AuthInfo — the in-memory dialer shim
	// pkg/wire.Dial uses on the single-box unix path returns this
	// shape from the no-credentials handshake.
	ctx := peer.NewContext(context.Background(), &peer.Peer{Addr: &net.UnixAddr{Name: "/tmp/x", Net: "unix"}})
	_, err := PeerCN(ctx)
	if !errors.Is(err, ErrPeerCNUnavailable) {
		t.Fatalf("PeerCN(non-TLS peer) = %v, want ErrPeerCNUnavailable", err)
	}
}

// TestPeerCN_UnitOnTLSPeerEmptyChain verifies the helper refuses
// TLSInfo with no verified chains — a defensive pin for the case
// where a hand-crafted client/server cert slips past handshake
// validation but PeerCN's chain-empty short-circuit fires.
func TestPeerCN_UnitOnTLSPeerEmptyChain(t *testing.T) {
	ctx := peer.NewContext(context.Background(), &peer.Peer{
		Addr:     &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1},
		AuthInfo: credentials.TLSInfo{},
	})
	_, err := PeerCN(ctx)
	if !errors.Is(err, ErrPeerCNUnavailable) {
		t.Fatalf("PeerCN(TLSInfo empty chain) = %v, want ErrPeerCNUnavailable", err)
	}
}

// capturingHealthServer is the minimum surface of *health.Server we
// need: a Check RPC that responds SERVING. We could just call
// health.NewServer() but rolling our own lets the test keep
// minimal imports + a single registration site.
type capturingHealthServer struct {
	healthgrpc.UnimplementedHealthServer
	serving bool
}

func newCapturingHealthServer() *capturingHealthServer { return &capturingHealthServer{} }

func (s *capturingHealthServer) setServing() { s.serving = true }

func (s *capturingHealthServer) Check(_ context.Context, _ *healthgrpc.HealthCheckRequest) (*healthgrpc.HealthCheckResponse, error) {
	if !s.serving {
		return nil, errors.New("capturing health: not yet serving")
	}
	return &healthgrpc.HealthCheckResponse{Status: healthgrpc.HealthCheckResponse_SERVING}, nil
}

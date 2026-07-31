// mtls_e2e_test.go — cross-process mTLS round-trip over a real TCP listener
// (ADR-052 §Test surface). The pkg/wire/peer_test.go unit tests cover the
// in-process handshake; this file covers the load-bearing gap that bufconn
// hides: a real OS TCP listener, a real *tls.Config, and the full pkg/pki
// → wire.DialContext → wire.ServerCredsOrEmpty → PeerCN chain.
//
// What this tests:
//   1. Gregale pki init produces a CA + per-daemon leaves that round-trip
//      over a real TCP listener on 127.0.0.1 (no KVM, no Postgres).
//   2. The handler-layer PeerCN lookup returns the client's CN only after
//      a successful handshake (regression-pin for the tls.NewListener
//      removal that motivated this slice).
//
// Build tag: (none). CI-safe. Runs under `make test`. Skips when
// FAAS_SKIP_MTLS_E2E is set (matches the FAAS_SKIP_PG_TESTS pattern
// from cmd/e2e/quota_e2e_test.go).

package e2e_test

import (
	"context"
	"os"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/peer"

	"github.com/onebox-faas/faas/pkg/pki"
	"github.com/onebox-faas/faas/pkg/wire"
)

// skipUnlessMTLS skips the test when the operator sets FAAS_SKIP_MTLS_E2E.
// Matches the FAAS_SKIP_PG_TESTS pattern from cmd/e2e/quota_e2e_test.go.
func skipUnlessMTLS(t *testing.T) {
	t.Helper()
	if os.Getenv("FAAS_SKIP_MTLS_E2E") != "" {
		t.Skip("FAAS_SKIP_MTLS_E2E set; skipping mTLS e2e")
	}
}

// TestMTLSE2E_PKIBootstrapLandsExpectedLayout verifies the operator-side
// CLI path lands material on disk in the expected layout. The e2e
// round-trip below depends on this — without CA + leaves, there's no
// chain to verify.
func TestMTLSE2E_PKIBootstrapLandsExpectedLayout(t *testing.T) {
	skipUnlessMTLS(t)

	root := t.TempDir()

	// Bootstrap a CA, then issue every per-daemon leaf defined in
	// pkg/pki.Roles(). Mirrors what `gregale pki init` does on a
	// production box — we go through pkg/pki directly here so the
	// test doesn't depend on the gregale binary being installed.
	caCert, caKey, err := pki.EnsureCA(root, false)
	if err != nil {
		t.Fatalf("EnsureCA: %v", err)
	}
	for _, role := range pki.Roles() {
		if err := pki.EnsureLeaf(root, role, caCert, caKey, false); err != nil {
			t.Fatalf("EnsureLeaf %s/%s: %v", role.Directory, role.Filename, err)
		}
	}

	// Every Role.AltNames entry must be reachable on disk; the SAN
	// list is what the stdlib verifier checks during the handshake.
	for _, role := range pki.Roles() {
		certPath, keyPath := pki.LeafPaths(root, role)
		if _, err := os.Stat(certPath); err != nil {
			t.Errorf("leaf %s missing: %v", certPath, err)
		}
		if _, err := os.Stat(keyPath); err != nil {
			t.Errorf("key %s missing: %v", keyPath, err)
		}
	}
}

// TestMTLSE2E_RoundTripOnRealTCP exercises the full mTLS path on a real
// 127.0.0.1 TCP listener (no bufconn). This is the only test that proves
// the cert path works on a real OS listener — bufconn hides handshake
// bugs that show up only on the wire.
func TestMTLSE2E_RoundTripOnRealTCP(t *testing.T) {
	skipUnlessMTLS(t)

	fx := bootstrapPKI(t)
	serverTLS, err := wire.LoadServerTLSConfigWithPrefix("server_", fx.serverCert, fx.serverKey, fx.caCert)
	if err != nil {
		t.Fatalf("LoadServerTLSConfig: %v", err)
	}
	clientTLS, err := wire.LoadClientTLSConfigWithPrefix("client_", fx.clientCert, fx.clientKey, fx.caCert)
	if err != nil {
		t.Fatalf("LoadClientTLSConfig: %v", err)
	}

	// Bind on a free 127.0.0.1 port via wire.Listen (real TCP —
	// not bufconn, not a net.Pipe).
	lis, err := wire.Listen(context.Background(), "tcp://127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = lis.Close() })

	// Capture the peer's CN via a unary interceptor. Reading PeerCN
	// here is the exact contract ADR-052 §Handler-layer peer binding
	// relies on; if this fails, the ServerCredsOrEmpty wiring is
	// broken (the load-bearing regression that motivated this slice).
	var capturedCN string
	var sawTLSInfo bool
	interceptor := func(ctx context.Context, req interface{}, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if p, ok := peer.FromContext(ctx); ok {
			if _, ok := p.AuthInfo.(credentials.TLSInfo); ok {
				sawTLSInfo = true
			}
		}
		if cn, perr := wire.PeerCN(ctx); perr == nil {
			capturedCN = cn
		}
		return handler(ctx, req)
	}

	opts := []grpc.ServerOption{grpc.UnaryInterceptor(interceptor)}
	opts = append(opts, wire.ServerCredsOrEmpty(serverTLS)...)
	srv := grpc.NewServer(opts...)
	hs := &mtlsHealthServer{}
	healthgrpc.RegisterHealthServer(srv, hs)
	hs.serving = true
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { srv.Stop() })

	// Dial the same listener with the client cert. The handshake
	// must succeed: chain trust (both signed by fx.caCert), SAN
	// match (server cert carries 127.0.0.1 + localhost), EKU
	// (server leaf has ServerAuth, client leaf has ClientAuth).
	conn, err := wire.Dial(context.Background(), "tcp://"+lis.Addr().String(), clientTLS)
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
		t.Fatal("peer.AuthInfo did not carry credentials.TLSInfo (gRPC server transport should populate it on every TLS connection)")
	}
	if capturedCN == "" {
		t.Fatal("PeerCN returned empty CN — handler-layer CN binding is broken")
	}
	// The server sees the client's CN, not the server's CN. The
	// client cert's CN is "mtls-e2e-client" (bootstrapPKI).
	if capturedCN != "mtls-e2e-client" {
		t.Errorf("PeerCN = %q, want %q", capturedCN, "mtls-e2e-client")
	}
}

// --- fixtures -------------------------------------------------------------

type mtlsPKI struct {
	rootDir    string
	caCert     string
	caKey      string
	serverCert string
	serverKey  string
	clientCert string
	clientKey  string
}

// bootstrapPKI lays out a minimal CA + server + client leaf under a
// per-test TempDir. Mirrors what `gregale pki init` does in production,
// but with a single-server + single-client pair because that's all
// the e2e needs.
func bootstrapPKI(t *testing.T) mtlsPKI {
	t.Helper()
	root := t.TempDir()

	caCert, caKey, err := pki.EnsureCA(root, false)
	if err != nil {
		t.Fatalf("EnsureCA: %v", err)
	}
	caCertPath, caKeyPath := pki.CARoot(root)

	serverRole := pki.Role{
		CommonName: "mtls-e2e-server",
		Kind:       pki.KindServer,
		Directory:  "server",
		Filename:   "server",
		AltNames:   pki.LocalDevSANs(),
	}
	if err := pki.EnsureLeaf(root, serverRole, caCert, caKey, false); err != nil {
		t.Fatalf("EnsureLeaf server: %v", err)
	}
	serverCert, serverKey := pki.LeafPaths(root, serverRole)

	clientRole := pki.Role{
		CommonName: "mtls-e2e-client",
		Kind:       pki.KindClient,
		Directory:  "client",
		Filename:   "client",
		AltNames:   pki.LocalDevSANs(),
	}
	if err := pki.EnsureLeaf(root, clientRole, caCert, caKey, false); err != nil {
		t.Fatalf("EnsureLeaf client: %v", err)
	}
	clientCert, clientKey := pki.LeafPaths(root, clientRole)

	return mtlsPKI{
		rootDir:    root,
		caCert:     caCertPath,
		caKey:      caKeyPath,
		serverCert: serverCert,
		serverKey:  serverKey,
		clientCert: clientCert,
		clientKey:  clientKey,
	}
}

// mtlsHealthServer is the minimum surface of *health.Server we need
// for the round-trip test. Mirrors the pattern in pkg/wire/peer_test.go.
type mtlsHealthServer struct {
	healthgrpc.UnimplementedHealthServer
	serving bool
}

func (s *mtlsHealthServer) Check(_ context.Context, _ *healthgrpc.HealthCheckRequest) (*healthgrpc.HealthCheckResponse, error) {
	if !s.serving {
		return nil, nil
	}
	return &healthgrpc.HealthCheckResponse{Status: healthgrpc.HealthCheckResponse_SERVING}, nil
}

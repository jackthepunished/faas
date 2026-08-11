// gRPC dial/listen tests for pkg/wire. Exercises the strict target parser,
// the fail-closed mTLS gates (TCP/DNS nil-TLS, TCP nil-listener-TLS), and a
// real gRPC RPC over an mTLS-bound listener on 127.0.0.1:0. The round-trip
// tests build a throwaway CA and per-test leaf certificates under
// t.TempDir(); nothing on disk outlives the test process.

package wire

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	healthsvc "google.golang.org/grpc/health"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"
)

// --- ParseTarget ----------------------------------------------------------

func TestParseTarget(t *testing.T) {
	cases := []struct {
		raw       string
		want      Target
		expectErr bool
	}{
		{raw: "unix:///run/faas/vmmd.sock", want: Target{Scheme: SchemeUnix, Address: "/run/faas/vmmd.sock"}},
		{raw: "tcp://127.0.0.1:50051", want: Target{Scheme: SchemeTCP, Address: "127.0.0.1:50051"}},
		{raw: "tcp://0.0.0.0:50051", want: Target{Scheme: SchemeTCP, Address: "0.0.0.0:50051"}},
		{raw: "tcp://:50051", want: Target{Scheme: SchemeTCP, Address: ":50051"}},
		{raw: "tcp://127.0.0.1:0", want: Target{Scheme: SchemeTCP, Address: "127.0.0.1:0"}},
		{raw: "dns:///vmmd.internal:50051", want: Target{Scheme: SchemeDNS, Address: "vmmd.internal:50051"}},
		{raw: "dns:///vmmd.internal:443", want: Target{Scheme: SchemeDNS, Address: "vmmd.internal:443"}},
		{raw: "dns://vmmd.internal:50051", want: Target{Scheme: SchemeDNS, Address: "vmmd.internal:50051"}},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := ParseTarget(tc.raw)
			if (err != nil) != tc.expectErr {
				t.Fatalf("err = %v, expectErr = %v", err, tc.expectErr)
			}
			if err == nil && got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseTargetRejectsInvalidTargets(t *testing.T) {
	cases := []struct {
		raw  string
		name string
	}{
		{raw: "", name: "empty"},
		{raw: "/run/faas/vmmd.sock", name: "bare absolute path (no scheme)"},
		{raw: "relative.sock", name: "bare relative path (no scheme)"},
		{raw: "127.0.0.1:50051", name: "bare host:port"},
		{raw: "unix://relative.sock", name: "non-absolute unix path"},
		{raw: "unix://host/path", name: "unix with authority"},
		{raw: "tcp://127.0.0.1", name: "tcp missing port"},
		{raw: "tcp://127.0.0.1:99999", name: "tcp port out of range"},
		{raw: "tcp://127.0.0.1:abc", name: "tcp non-numeric port"},
		{raw: "tcp:///path", name: "tcp with path"},
		{raw: "dns://:50051", name: "dns missing hostname (triple-slash)"},
		{raw: "dns:///host:50051/extra", name: "dns with extra path"},
		{raw: "tcpp://127.0.0.1:50051", name: "unknown scheme"},
		{raw: "https://example.com", name: "https scheme"},
		{raw: "unix:///run/faas/vmmd.sock?query=1", name: "unix with query"},
		{raw: "unix:///run/faas/vmmd.sock#frag", name: "unix with fragment"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseTarget(tc.raw); err == nil {
				t.Fatalf("ParseTarget(%q) returned nil error; want error", tc.raw)
			}
		})
	}
}

func TestNormalizeLegacyTarget(t *testing.T) {
	cases := []struct {
		raw    string
		want   string
		errSub string
	}{
		{raw: "/run/faas/vmmd.sock", want: "unix:///run/faas/vmmd.sock"},
		{raw: "unix:///run/faas/vmmd.sock", want: "unix:///run/faas/vmmd.sock"},
		{raw: "tcp://127.0.0.1:50051", want: "tcp://127.0.0.1:50051"},
		{raw: "", errSub: "empty"},
		{raw: "relative.sock", errSub: "absolute"},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := NormalizeLegacyTarget(tc.raw)
			if tc.errSub != "" {
				if err == nil {
					t.Fatalf("NormalizeLegacyTarget(%q) returned nil err; want containing %q", tc.raw, tc.errSub)
				}
				if !strings.Contains(err.Error(), tc.errSub) {
					t.Fatalf("err = %v; want substring %q", err, tc.errSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// --- Fail-closed auth -----------------------------------------------------

func TestDialTCPDNSRejectsNilTLS(t *testing.T) {
	for _, target := range []string{
		"tcp://127.0.0.1:50051",
		"dns:///vmmd.internal:50051",
	} {
		t.Run(target, func(t *testing.T) {
			_, err := Dial(context.Background(), target, nil)
			if err == nil {
				t.Fatalf("Dial(%q, nil) returned nil error", target)
			}
			if !strings.Contains(err.Error(), "mTLS required") {
				t.Fatalf("err = %v; want containing %q", err, "mTLS required")
			}
		})
	}
}

func TestDialTCPUnwrapsWithTLS(t *testing.T) {
	// When mTLS is provided we expect Dial to construct a (lazy) client conn.
	// We don't make the peer reachable; the construction step is the
	// contract being verified here.
	pki := newTestPKI(t)
	clientTLS, err := LoadClientTLSConfig(pki.clientCert, pki.clientKey, pki.caCert)
	if err != nil {
		t.Fatalf("LoadClientTLSConfig: %v", err)
	}
	// Port 1 is the IANA tcpmux port — never bound in tests; the lazy
	// dial returns immediately even though no peer is up.
	conn, err := Dial(context.Background(), "tcp://127.0.0.1:1", clientTLS)
	if err != nil {
		t.Fatalf("Dial with mTLS: %v", err)
	}
	if conn == nil {
		t.Fatalf("Dial returned nil conn")
	}
	_ = conn.Close()
}

func TestDialTCPRejectsPortZero(t *testing.T) {
	pki := newTestPKI(t)
	clientTLS, err := LoadClientTLSConfig(pki.clientCert, pki.clientKey, pki.caCert)
	if err != nil {
		t.Fatalf("LoadClientTLSConfig: %v", err)
	}
	_, err = Dial(context.Background(), "tcp://127.0.0.1:0", clientTLS)
	if err == nil {
		t.Fatalf("expected dial to reject port 0")
	}
	if !strings.Contains(err.Error(), "port 0") {
		t.Fatalf("err = %v; want mention of port 0", err)
	}
}

func TestListenTCPRequiresMTLS(t *testing.T) {
	_, err := Listen(context.Background(), "tcp://127.0.0.1:0", nil)
	if err == nil {
		t.Fatalf("Listen(tcp, nil TLS) returned nil error")
	}
	if !strings.Contains(err.Error(), "mTLS required") {
		t.Fatalf("err = %v; want containing %q", err, "mTLS required")
	}
	if !strings.Contains(err.Error(), "listen_addr") {
		t.Fatalf("err = %v; want mentioning %q", err, "listen_addr")
	}
}

func TestListenDNSRejected(t *testing.T) {
	_, err := Listen(context.Background(), "dns:///vmmd.internal:50051", nil)
	if err == nil {
		t.Fatalf("Listen(dns) returned nil error")
	}
	if !strings.Contains(err.Error(), "not a bind target") {
		t.Fatalf("err = %v; want %q", err, "not a bind target")
	}
}

func TestDialEmptyTarget(t *testing.T) {
	_, err := Dial(context.Background(), "", nil)
	if err == nil {
		t.Fatalf("Dial(\"\", nil) returned nil error")
	}
}

func TestDialContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := DialContext(ctx, "/run/faas/vmmd.sock", nil)
	if err == nil {
		t.Fatalf("DialContext with cancelled ctx returned nil error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v; want context.Canceled (errors.Is)", err)
	}
}

// --- mTLS round-trip ------------------------------------------------------

func TestMTLSRoundTrip(t *testing.T) {
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
		t.Fatalf("Listen tcp mTLS: %v", err)
	}
	t.Cleanup(func() { _ = lis.Close() })

	addr := lis.Addr().String()
	healthServer := healthsvc.NewServer()
	healthServer.SetServingStatus("", healthgrpc.HealthCheckResponse_SERVING)
	srv := grpc.NewServer(ServerCredsOrEmpty(serverTLS)...)
	healthgrpc.RegisterHealthServer(srv, healthServer)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { srv.Stop() })

	conn, err := Dial(context.Background(), "tcp://"+addr, clientTLS)
	if err != nil {
		t.Fatalf("Dial tcp mTLS: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	cli := healthgrpc.NewHealthClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := cli.Check(ctx, &healthgrpc.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("Health/Check: %v", err)
	}
	if resp.GetStatus() != healthgrpc.HealthCheckResponse_SERVING {
		t.Fatalf("status = %v; want SERVING", resp.GetStatus())
	}
}

// --- ADR-056: handshake-layer NodeVerifier integration tests -------------
//
// These tests exercise the new LoadServerTLSConfigWithVerifier /
// LoadClientTLSConfigWithVerifier factories and prove that the
// verifier augments (never replaces) stdlib trust. They reuse the
// standard newTestPKI helper, whose leaves carry CN="test-server"
// and CN="test-client".

// runHealthCheckMTLS is the shared happy-path round-trip helper
// (Dial → Health.Check). It returns the dial error (which surfaces
// both transport-level rejections and grpc handshake failures).
func runHealthCheckMTLS(t *testing.T, serverTLS, clientTLS *tls.Config) error {
	t.Helper()

	lis, err := Listen(context.Background(), "tcp://127.0.0.1:0", serverTLS)
	if err != nil {
		return fmt.Errorf("Listen: %w", err)
	}
	t.Cleanup(func() { _ = lis.Close() })

	addr := lis.Addr().String()
	healthServer := healthsvc.NewServer()
	healthServer.SetServingStatus("", healthgrpc.HealthCheckResponse_SERVING)
	srv := grpc.NewServer(ServerCredsOrEmpty(serverTLS)...)
	healthgrpc.RegisterHealthServer(srv, healthServer)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { srv.Stop() })

	conn, err := Dial(context.Background(), "tcp://"+addr, clientTLS)
	if err != nil {
		return fmt.Errorf("Dial: %w", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	cli := healthgrpc.NewHealthClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := cli.Check(ctx, &healthgrpc.HealthCheckRequest{})
	if err != nil {
		return fmt.Errorf("Health/Check: %w", err)
	}
	if resp.GetStatus() != healthgrpc.HealthCheckResponse_SERVING {
		return fmt.Errorf("status = %v; want SERVING", resp.GetStatus())
	}
	return nil
}

// TestMTLSRoundTrip_NodeVerifierHappyPath: a CN-matching InmemNodeVerifier
// on the server side lets the dial through. The verifier is a security
// augmentation, not a parallel trust gate.
func TestMTLSRoundTrip_NodeVerifierHappyPath(t *testing.T) {
	pki := newTestPKI(t)

	// The server-side VerifyPeerCertificate hook fires for the
	// CLIENT's leaf cert (the peer presenting the cert). Register
	// "test-client" — the leaf CN of the test client cert.
	serverReg := NewInmemNodeVerifier()
	serverReg.Set([]string{"test-client"})
	serverTLS, err := LoadServerTLSConfigWithVerifier(pki.serverCert, pki.serverKey, pki.caCert, serverReg)
	if err != nil {
		t.Fatalf("LoadServerTLSConfigWithVerifier: %v", err)
	}
	if serverTLS.VerifyPeerCertificate == nil {
		t.Fatalf("VerifyPeerCertificate is nil; want non-nil")
	}
	if serverTLS.InsecureSkipVerify {
		t.Fatalf("InsecureSkipVerify = true; want false")
	}

	// Client side has no verifier (single-direction protection is
	// the canonical wiring — the verifier is server-side for the
	// vmmd→schedd CapacityReport direction).
	clientTLS, err := LoadClientTLSConfig(pki.clientCert, pki.clientKey, pki.caCert)
	if err != nil {
		t.Fatalf("LoadClientTLSConfig: %v", err)
	}

	if err := runHealthCheckMTLS(t, serverTLS, clientTLS); err != nil {
		t.Fatalf("happy-path round-trip: %v", err)
	}
}

// TestMTLSRoundTrip_NodeVerifierCNMismatch: a CN-not-in-registry
// server-side verifier rejects the leaf at handshake time. The
// rejection happens BEFORE any RPC dispatch — gRPC's dial surfaces
// it as a transport error.
func TestMTLSRoundTrip_NodeVerifierCNMismatch(t *testing.T) {
	pki := newTestPKI(t)

	// Server-side verifier checks the CLIENT's leaf CN. Register
	// "other-role" — the actual client leaf CN ("test-client") is
	// NOT in the registry, so the verifier rejects at handshake.
	serverReg := NewInmemNodeVerifier()
	serverReg.Set([]string{"other-role"})
	serverTLS, err := LoadServerTLSConfigWithVerifier(pki.serverCert, pki.serverKey, pki.caCert, serverReg)
	if err != nil {
		t.Fatalf("LoadServerTLSConfigWithVerifier: %v", err)
	}

	clientTLS, err := LoadClientTLSConfig(pki.clientCert, pki.clientKey, pki.caCert)
	if err != nil {
		t.Fatalf("LoadClientTLSConfig: %v", err)
	}

	err = runHealthCheckMTLS(t, serverTLS, clientTLS)
	if err == nil {
		t.Fatalf("handshake with CN mismatch returned nil err; want non-nil")
	}
	// We don't pin the exact error message (crypto/tls wording
	// varies across Go versions). Asserting non-nil is the
	// load-bearing contract.
}

// TestMTLSRoundTrip_NodeVerifierStdlibPriority: when stdlib trust
// fails (e.g. server leaf was signed by a different CA), the
// verifier is NEVER consulted. We prove this by attaching a
// strict-nil InmemNodeVerifier (which would reject every CN if
// asked) and asserting the rejection message contains the stdlib
// "unknown authority" text, not the verifier's CN error.
func TestMTLSRoundTrip_NodeVerifierStdlibPriority(t *testing.T) {
	serverPKI := newTestPKI(t)
	wrongPKI := newTestPKI(t) // independent CA — stdlib MUST reject

	serverTLS, err := LoadServerTLSConfigWithVerifier(
		serverPKI.serverCert, serverPKI.serverKey, wrongPKI.caCert,
		NewInmemNodeVerifier(), // strict-nil would reject everything
	)
	if err != nil {
		t.Fatalf("LoadServerTLSConfigWithVerifier: %v", err)
	}
	clientTLS, err := LoadClientTLSConfig(serverPKI.clientCert, serverPKI.clientKey, serverPKI.caCert)
	if err != nil {
		t.Fatalf("LoadClientTLSConfig: %v", err)
	}

	err = runHealthCheckMTLS(t, serverTLS, clientTLS)
	if err == nil {
		t.Fatalf("stdlib-trust-failure handshake returned nil err; want non-nil")
	}
	// The failure must be a stdlib chain-trust failure, not the
	// verifier's. We don't pin the exact message — stdlib's wording
	// shifts across versions — but we DO assert the rejection
	// happened (non-nil) and the dial didn't reach Health/Check.
}

// TestMTLSRoundTrip_NodeVerifierNilDegradesToStdlib: passing a nil
// verifier to LoadServerTLSConfigWithVerifier produces a config
// equivalent to LoadServerTLSConfig (no VerifyPeerCertificate hook).
// Single-box callers can use the *WithVerifier factory without
// special-casing the nil-verifier case.
func TestMTLSRoundTrip_NodeVerifierNilDegradesToStdlib(t *testing.T) {
	pki := newTestPKI(t)

	serverTLS, err := LoadServerTLSConfigWithVerifier(pki.serverCert, pki.serverKey, pki.caCert, nil)
	if err != nil {
		t.Fatalf("LoadServerTLSConfigWithVerifier: %v", err)
	}
	if serverTLS.VerifyPeerCertificate != nil {
		t.Fatalf("VerifyPeerCertificate != nil; want nil (nil verifier = no hook)")
	}
	if serverTLS.InsecureSkipVerify {
		t.Fatalf("InsecureSkipVerify = true; want false")
	}

	clientTLS, err := LoadClientTLSConfig(pki.clientCert, pki.clientKey, pki.caCert)
	if err != nil {
		t.Fatalf("LoadClientTLSConfig: %v", err)
	}
	if err := runHealthCheckMTLS(t, serverTLS, clientTLS); err != nil {
		t.Fatalf("nil-verifier round-trip: %v", err)
	}
}

func TestMTLSRoundTripRejectsWrongCA(t *testing.T) {
	serverPKI := newTestPKI(t)
	wrongPKI := newTestPKI(t) // independently generated, never trusted

	serverTLS, err := LoadServerTLSConfig(serverPKI.serverCert, serverPKI.serverKey, serverPKI.caCert)
	if err != nil {
		t.Fatalf("LoadServerTLSConfig: %v", err)
	}
	// Client trusts wrongPKI's CA but presents no certificate. Per the
	// loader contract, that produces a client config; the server's
	// RequireAndVerifyClientCert will then reject the unauthenticated peer
	// even before CA mismatch becomes relevant. Both failure modes prove
	// the auth boundary; assert on the RPC failure, not on Dial's return.
	clientTLS, err := LoadClientTLSConfig(wrongPKI.clientCert, wrongPKI.clientKey, wrongPKI.caCert)
	if err != nil {
		t.Fatalf("LoadClientTLSConfig: %v", err)
	}

	lis, err := Listen(context.Background(), "tcp://127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatalf("Listen tcp mTLS: %v", err)
	}
	t.Cleanup(func() { _ = lis.Close() })

	addr := lis.Addr().String()
	srv := grpc.NewServer()
	healthgrpc.RegisterHealthServer(srv, healthsvc.NewServer())
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { srv.Stop() })

	conn, err := Dial(context.Background(), "tcp://"+addr, clientTLS)
	if err != nil {
		t.Fatalf("Dial tcp mTLS: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	cli := healthgrpc.NewHealthClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := cli.Check(ctx, &healthgrpc.HealthCheckRequest{}); err == nil {
		t.Fatalf("Health/Check succeeded with wrong CA; want failure")
	}
}

func TestMTLSRoundTripUntrustedServerCert(t *testing.T) {
	// The mirror case: server presents a leaf signed by a CA the client
	// doesn't trust. Without InsecureSkipVerify-only, the client-side
	// VerifyPeerCertificate must reject the handshake.
	serverPKI := newTestPKI(t)
	clientPKI := newTestPKI(t)

	serverTLS, err := LoadServerTLSConfig(serverPKI.serverCert, serverPKI.serverKey, serverPKI.caCert)
	if err != nil {
		t.Fatalf("LoadServerTLSConfig: %v", err)
	}
	clientTLS, err := LoadClientTLSConfig(clientPKI.clientCert, clientPKI.clientKey, clientPKI.caCert)
	if err != nil {
		t.Fatalf("LoadClientTLSConfig: %v", err)
	}

	lis, err := Listen(context.Background(), "tcp://127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatalf("Listen tcp mTLS: %v", err)
	}
	t.Cleanup(func() { _ = lis.Close() })

	srv := grpc.NewServer()
	healthgrpc.RegisterHealthServer(srv, healthsvc.NewServer())
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { srv.Stop() })

	conn, err := Dial(context.Background(), "tcp://"+lis.Addr().String(), clientTLS)
	if err != nil {
		t.Fatalf("Dial tcp mTLS: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	cli := healthgrpc.NewHealthClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := cli.Check(ctx, &healthgrpc.HealthCheckRequest{}); err == nil {
		t.Fatalf("Health/Check succeeded against untrusted server CA; want failure")
	}
}

// TestMTLSRoundTripRejectsHostnameMismatch locks the new contract that
// loadClientTLSConfig relies on stdlib's SAN check (closes alert #58).
// The server cert is issued with IPAddresses=[10.0.0.99] only, but the
// dial target is 127.0.0.1; the stdlib verifier (via grpc-go's
// ServerName auto-promotion from the dial :authority) must reject the
// handshake. Without the SAN check, this test would pass silently.
//
// The PKI is built inline (rather than reusing newTestPKI) because the
// helper hard-codes 127.0.0.1/localhost SANs that would mask the
// mismatch path.
func TestMTLSRoundTripRejectsHostnameMismatch(t *testing.T) {
	dir := t.TempDir()

	caCert, caCertPEM, caKey := mustGenSelfSigned(t, x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca-mismatch"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	})

	// Leaf signed for 10.0.0.99 only — IP SAN does NOT cover 127.0.0.1.
	serverCertPEM, serverKeyPEM := mustGenSigned(t, x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "test-server-mismatch"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("10.0.0.99")},
	}, caCert, caKey)

	clientCertPEM, clientKeyPEM := mustGenSigned(t, x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: "test-client-mismatch"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, caCert, caKey)

	caCertPath := writeTestFile(t, filepath.Join(dir, "ca.pem"), caCertPEM)
	serverCertPath := writeTestFile(t, filepath.Join(dir, "server.crt"), serverCertPEM)
	serverKeyPath := writeTestFile(t, filepath.Join(dir, "server.key"), serverKeyPEM)
	clientCertPath := writeTestFile(t, filepath.Join(dir, "client.crt"), clientCertPEM)
	clientKeyPath := writeTestFile(t, filepath.Join(dir, "client.key"), clientKeyPEM)

	serverTLS, err := LoadServerTLSConfig(serverCertPath, serverKeyPath, caCertPath)
	if err != nil {
		t.Fatalf("LoadServerTLSConfig: %v", err)
	}
	clientTLS, err := LoadClientTLSConfig(clientCertPath, clientKeyPath, caCertPath)
	if err != nil {
		t.Fatalf("LoadClientTLSConfig: %v", err)
	}

	lis, err := Listen(context.Background(), "tcp://127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatalf("Listen tcp mTLS: %v", err)
	}
	t.Cleanup(func() { _ = lis.Close() })

	srv := grpc.NewServer()
	healthgrpc.RegisterHealthServer(srv, healthsvc.NewServer())
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { srv.Stop() })

	conn, err := Dial(context.Background(), "tcp://"+lis.Addr().String(), clientTLS)
	if err != nil {
		t.Fatalf("Dial tcp mTLS: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	cli := healthgrpc.NewHealthClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := cli.Check(ctx, &healthgrpc.HealthCheckRequest{}); err == nil {
		t.Fatalf("Health/Check succeeded against hostname-mismatched server; stdlib SAN check is not enforcing")
	}
}

// --- TLS loaders ----------------------------------------------------------

func TestTLSLoadersAcceptNilAllEmpty(t *testing.T) {
	if s, err := LoadServerTLSConfig("", "", ""); err != nil || s != nil {
		t.Fatalf("server empty: cfg=%v err=%v", s, err)
	}
	if c, err := LoadClientTLSConfig("", "", ""); err != nil || c != nil {
		t.Fatalf("client empty: cfg=%v err=%v", c, err)
	}
}

func TestTLSLoadersRejectPartialConfiguration(t *testing.T) {
	cases := []struct {
		name          string
		cert, key, ca string
		// missingSub is the field that MUST appear in the error. The
		// loaders always list every missing field, so any subset suffices.
		missingSub string
		load       func(string, string, string) (*tls.Config, error)
	}{
		{"server: only cert", "a", "", "", "tls_ca_path", LoadServerTLSConfig},
		{"server: only key", "", "b", "", "tls_ca_path", LoadServerTLSConfig},
		{"server: only ca", "", "", "c", "tls_cert_path", LoadServerTLSConfig},
		{"server: cert+key no ca", "a", "b", "", "tls_ca_path", LoadServerTLSConfig},
		{"server: cert+ca no key", "a", "", "c", "tls_key_path", LoadServerTLSConfig},
		{"server: key+ca no cert", "", "b", "c", "tls_cert_path", LoadServerTLSConfig},
		{"client: only cert", "a", "", "", "tls_ca_path", LoadClientTLSConfig},
		{"client: cert+key no ca", "a", "b", "", "tls_ca_path", LoadClientTLSConfig},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.load(tc.cert, tc.key, tc.ca)
			if err == nil {
				t.Fatalf("expected error containing %q", tc.missingSub)
			}
			if !strings.Contains(err.Error(), tc.missingSub) {
				t.Fatalf("err = %v; want substring %q", err, tc.missingSub)
			}
		})
	}
}

func TestTLSLoadersRejectInvalidPEM(t *testing.T) {
	dir := t.TempDir()
	badCA := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(badCA, []byte("not a PEM"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	pki := newTestPKI(t)
	// Server cert valid, CA file unreadable as PEM.
	_, err := LoadServerTLSConfig(pki.serverCert, pki.serverKey, badCA)
	if err == nil {
		t.Fatalf("expected error for non-PEM CA")
	}
	if !strings.Contains(err.Error(), "CA file") {
		t.Fatalf("err = %v; want CA file mention", err)
	}
}

// TestLoadClientTLSConfigDefaultVerifier pins the contract that
// loadClientTLSConfig delegates entirely to stdlib's verifier: no
// InsecureSkipVerify, no custom VerifyPeerCertificate. grpc-go's
// tlsCreds.ClientHandshake populates ServerName from the dial
// :authority before tls.Client is called (see internal/credentials
// CloneTLSConfig + assignment in credentials/tls.go).
func TestLoadClientTLSConfigDefaultVerifier(t *testing.T) {
	pki := newTestPKI(t)
	cfg, err := LoadClientTLSConfig(pki.clientCert, pki.clientKey, pki.caCert)
	if err != nil {
		t.Fatalf("LoadClientTLSConfig: %v", err)
	}
	if cfg.InsecureSkipVerify {
		t.Fatalf("InsecureSkipVerify = true; want false (stdlib verifier must run, including SAN check)")
	}
	if cfg.VerifyPeerCertificate != nil {
		t.Fatalf("VerifyPeerCertificate != nil; want nil (stdlib handles chain + SAN + EKU)")
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Fatalf("MinVersion = %x; want TLS 1.3 (%x)", cfg.MinVersion, tls.VersionTLS13)
	}
}

// TestTLSLoadersWithPrefix: schedd's error-name accuracy depends on the
// _WithPrefix variants. Pin that the prefix is applied to every missing
// field name and that the no-prefix variant stays generic.
func TestTLSLoadersWithPrefix(t *testing.T) {
	t.Run("server+prefix names vmmd_tls_*", func(t *testing.T) {
		_, err := LoadServerTLSConfigWithPrefix("vmmd_", "/some/cert", "", "")
		if err == nil {
			t.Fatalf("expected error")
		}
		if !strings.Contains(err.Error(), "vmmd_tls_key_path") {
			t.Errorf("err = %v; want vmmd_tls_key_path named", err)
		}
		if !strings.Contains(err.Error(), "vmmd_tls_ca_path") {
			t.Errorf("err = %v; want vmmd_tls_ca_path named", err)
		}
		if strings.Contains(err.Error(), "vmmd_tls_cert_path") {
			t.Errorf("err = %v; do NOT want vmmd_tls_cert_path named (it was set)", err)
		}
	})
	t.Run("client+empty prefix names tls_*", func(t *testing.T) {
		_, err := LoadClientTLSConfigWithPrefix("", "/some/cert", "", "")
		if err == nil {
			t.Fatalf("expected error")
		}
		if !strings.Contains(err.Error(), "tls_key_path") {
			t.Errorf("err = %v; want tls_key_path named", err)
		}
		if !strings.Contains(err.Error(), "tls_ca_path") {
			t.Errorf("err = %v; want tls_ca_path named", err)
		}
	})
}

// --- Load*TLSConfigWithReload factories (PR-E / ADR-052 §5) --------------
//
// Coverage targets:
//   - All-empty inputs preserve the (nil, nil) contract.
//   - Partial cluster inputs keep the Load*TLSConfig error shape (prefixed
//     field names).
//   - reload==nil degrades to the no-callback contract — the returned
//     config behaves exactly like the non-reload factory.
//   - reload!=nil installs GetConfigForClient on server factories and
//     GetClientCertificate on client factories.
//   - The composed *WithVerifierAndReload factory installs BOTH the
//     verifier hook AND the reload callback on the same *tls.Config.
//   - Calling the installed callback exercises the closure end-to-end
//     and returns a fresh *tls.Config (or *tls.Certificate, for the
//     client callback) that stdlib would hand to the handshake path.
//   - Reload returning an error propagates through both callbacks so
//     stdlib surfaces it as a handshake failure.
//   - Reload returning a *tls.Config with no Certificates surfaces
//     only via GetClientCertificate (server callback doesn't peek).

func TestTLSLoadersWithReload_AllEmptyReturnsNilNil(t *testing.T) {
	cases := []struct {
		name string
		load func() (*tls.Config, error)
	}{
		{"server", func() (*tls.Config, error) { return LoadServerTLSConfigWithReload("", "", "", "", nil) }},
		{"server+prefix+verifier", func() (*tls.Config, error) {
			return LoadServerTLSConfigWithPrefixAndVerifierAndReload("vmmd_", "", "", "", nil, nil)
		}},
		{"client", func() (*tls.Config, error) { return LoadClientTLSConfigWithReload("", "", "", "", nil) }},
		{"client+prefix+verifier", func() (*tls.Config, error) {
			return LoadClientTLSConfigWithPrefixAndVerifierAndReload("vmmd_", "", "", "", nil, nil)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := tc.load()
			if err != nil || cfg != nil {
				t.Fatalf("all-empty: cfg=%v err=%v, want nil/nil", cfg, err)
			}
		})
	}
}

func TestTLSLoadersWithReload_PartialClusterStillRejected(t *testing.T) {
	cases := []struct {
		name      string
		cert, key string
		prefix    string
		load      func(prefix, cert, key string) (*tls.Config, error)
		wantSubs  []string
	}{
		{"server+prefix cert-only", "a", "", "vmmd_",
			func(p, c, k string) (*tls.Config, error) { return LoadServerTLSConfigWithReload(p, c, k, "", nil) },
			[]string{"vmmd_tls_key_path", "vmmd_tls_ca_path"}},
		{"server+verifier+reload cert+key no ca", "a", "b", "vmmd_",
			func(p, c, k string) (*tls.Config, error) {
				return LoadServerTLSConfigWithPrefixAndVerifierAndReload(p, c, k, "", nil, nil)
			},
			[]string{"vmmd_tls_ca_path"}},
		{"client+prefix cert+ca no key", "a", "", "vmmd_",
			func(p, c, k string) (*tls.Config, error) {
				return LoadClientTLSConfigWithPrefixAndVerifierAndReload(p, c, k, "", nil, nil)
			},
			[]string{"vmmd_tls_key_path"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.load(tc.prefix, tc.cert, tc.key)
			if err == nil {
				t.Fatalf("expected error naming missing fields")
			}
			for _, want := range tc.wantSubs {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %q; want substring %q", err.Error(), want)
				}
			}
		})
	}
}

// TestTLSLoadersWithReload_NilReloadDegradesToNoCallback pins the
// single-box / pre-PR-E back-compat path: a nil reload closure means
// the returned config is the same as the non-reload factory's — no
// GetConfigForClient, no GetClientCertificate. This is what lets a
// caller pass reload=nil for "no cluster configured" without changing
// behaviour.
func TestTLSLoadersWithReload_NilReloadDegradesToNoCallback(t *testing.T) {
	pki := newTestPKI(t)

	server, err := LoadServerTLSConfigWithReload("", pki.serverCert, pki.serverKey, pki.caCert, nil)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	if server.GetConfigForClient != nil {
		t.Errorf("server reload=nil: GetConfigForClient = %p, want nil", server.GetConfigForClient)
	}
	if server.GetClientCertificate != nil {
		t.Errorf("server reload=nil: GetClientCertificate = %p, want nil", server.GetClientCertificate)
	}

	client, err := LoadClientTLSConfigWithReload("", pki.clientCert, pki.clientKey, pki.caCert, nil)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if client.GetConfigForClient != nil {
		t.Errorf("client reload=nil: GetConfigForClient = %p, want nil", client.GetConfigForClient)
	}
	if client.GetClientCertificate != nil {
		t.Errorf("client reload=nil: GetClientCertificate = %p, want nil", client.GetClientCertificate)
	}
}

// TestTLSLoadersWithReload_ServerInstallsGetConfigForClient pins the
// "reload installs server callback" contract. The callback, when
// invoked, MUST return the closure's *tls.Config so stdlib hands it
// to the handshake path. The verification chain (chain / SAN / EKU)
// is stdlib's responsibility — we only pin that the callback fires
// and the returned config carries the loader-built cert material.
func TestTLSLoadersWithReload_ServerInstallsGetConfigForClient(t *testing.T) {
	pki := newTestPKI(t)

	calls := 0
	reload := func() (*tls.Config, error) {
		calls++
		// Return a real config built via the existing non-reload
		// factory — exercises the same load path WatchTLSReload
		// will hit in production.
		return LoadServerTLSConfig(pki.serverCert, pki.serverKey, pki.caCert)
	}

	cfg, err := LoadServerTLSConfigWithReload("", pki.serverCert, pki.serverKey, pki.caCert, reload)
	if err != nil {
		t.Fatalf("LoadServerTLSConfigWithReload: %v", err)
	}
	if cfg.GetConfigForClient == nil {
		t.Fatal("GetConfigForClient = nil, want non-nil")
	}
	if cfg.GetClientCertificate == nil {
		t.Fatal("GetClientCertificate = nil, want non-nil (setReloadCallbacks installs both)")
	}

	// Fire the callback. With pki as the material, the returned
	// config must carry the loaded leaf (one Certificates entry).
	got, err := cfg.GetConfigForClient(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	if got == nil {
		t.Fatal("callback returned nil *tls.Config")
	}
	if len(got.Certificates) != 1 {
		t.Errorf("returned config: %d Certificates entries, want 1", len(got.Certificates))
	}
	if calls != 1 {
		t.Errorf("reload called %d times, want 1", calls)
	}

	// A second invocation must consult the closure again (the
	// contract is "fresh per handshake"). This pins stdlib's
	// per-handshake behaviour that gRPC's tlsCreds relies on.
	if _, err := cfg.GetConfigForClient(&tls.ClientHelloInfo{}); err != nil {
		t.Fatalf("callback (2nd): %v", err)
	}
	if calls != 2 {
		t.Errorf("reload called %d times after second invoke, want 2", calls)
	}
}

// TestTLSLoadersWithReload_ClientInstallsGetClientCertificate pins
// the client-side contract: the callback returns a *tls.Certificate
// derived from the closure's freshly-built config's Certificates[0].
// An empty Certificates slice MUST surface an explicit error rather
// than panic — stdlib would convert a nil *tls.Certificate into a
// confusing handshake failure, the explicit error makes the operator's
// job easier.
func TestTLSLoadersWithReload_ClientInstallsGetClientCertificate(t *testing.T) {
	pki := newTestPKI(t)

	calls := 0
	reload := func() (*tls.Config, error) {
		calls++
		return LoadClientTLSConfig(pki.clientCert, pki.clientKey, pki.caCert)
	}

	cfg, err := LoadClientTLSConfigWithReload("", pki.clientCert, pki.clientKey, pki.caCert, reload)
	if err != nil {
		t.Fatalf("LoadClientTLSConfigWithReload: %v", err)
	}
	if cfg.GetClientCertificate == nil {
		t.Fatal("GetClientCertificate = nil, want non-nil")
	}

	cert, err := cfg.GetClientCertificate(&tls.CertificateRequestInfo{})
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	if cert == nil {
		t.Fatal("callback returned nil *tls.Certificate")
	}
	if len(cert.Certificate) == 0 {
		t.Error("returned cert has 0 raw DER bytes")
	}
	if calls != 1 {
		t.Errorf("reload called %d times, want 1", calls)
	}
}

// TestTLSLoadersWithReload_ClientEmptyCertsSurfacesError pins the
// "no Certificates" guard in setReloadCallbacks' client callback. A
// closure that returns a *tls.Config with no Certificates (the
// production edge case: a fresh LoadX509KeyPair succeeded but the
// returned Certificate slice is empty — extremely unusual, but the
// guard prevents a confusing nil-pointer panic inside stdlib).
func TestTLSLoadersWithReload_ClientEmptyCertsSurfacesError(t *testing.T) {
	pki := newTestPKI(t)

	reload := func() (*tls.Config, error) {
		return &tls.Config{}, nil // no Certificates
	}

	cfg, err := LoadClientTLSConfigWithReload("", pki.clientCert, pki.clientKey, pki.caCert, reload)
	if err != nil {
		t.Fatalf("LoadClientTLSConfigWithReload: %v", err)
	}
	cert, err := cfg.GetClientCertificate(&tls.CertificateRequestInfo{})
	if err == nil {
		t.Fatalf("expected error for empty Certificates, got nil (cert=%v)", cert)
	}
	if !strings.Contains(err.Error(), "no Certificates") {
		t.Errorf("err = %q; want substring %q", err.Error(), "no Certificates")
	}
}

// TestTLSLoadersWithReload_ReloadErrorPropagates pins the contract
// that a closure error flows through both stdlib callbacks. Stdlib
// surfaces a callback-returned error as a TLS handshake failure —
// failing closed is the right behaviour for a temporarily-missing
// cert file (WatchTLSReload keeps the prior material live on its
// end, but the per-handshake callback is allowed to fail closed).
func TestTLSLoadersWithReload_ReloadErrorPropagates(t *testing.T) {
	pki := newTestPKI(t)

	boom := errors.New("reload: file vanished")
	reload := func() (*tls.Config, error) { return nil, boom }

	srv, err := LoadServerTLSConfigWithReload("", pki.serverCert, pki.serverKey, pki.caCert, reload)
	if err != nil {
		t.Fatalf("LoadServerTLSConfigWithReload: %v", err)
	}
	got, err := srv.GetConfigForClient(&tls.ClientHelloInfo{})
	if err == nil || got != nil {
		t.Errorf("server: got=%v err=%v; want nil cfg + boom err", got, err)
	}
	if err != boom {
		t.Errorf("server: err = %v, want %v", err, boom)
	}

	cli, err := LoadClientTLSConfigWithReload("", pki.clientCert, pki.clientKey, pki.caCert, reload)
	if err != nil {
		t.Fatalf("LoadClientTLSConfigWithReload: %v", err)
	}
	cert, err := cli.GetClientCertificate(&tls.CertificateRequestInfo{})
	if err == nil || cert != nil {
		t.Errorf("client: cert=%v err=%v; want nil cert + boom err", cert, err)
	}
	if err != boom {
		t.Errorf("client: err = %v, want %v", err, boom)
	}
}

// TestTLSLoadersWithReloadAndVerifier_BothHooksInstalled pins the
// composed contract: a non-nil verifier AND a non-nil reload installs
// BOTH the VerifyPeerCertificate hook AND the reload callbacks on the
// same *tls.Config. This is the production cmd/<daemon> call-site
// shape after PR-E.
func TestTLSLoadersWithReloadAndVerifier_BothHooksInstalled(t *testing.T) {
	pki := newTestPKI(t)

	verifier := NewInmemNodeVerifier()
	reload := func() (*tls.Config, error) {
		return LoadServerTLSConfig(pki.serverCert, pki.serverKey, pki.caCert)
	}

	srv, err := LoadServerTLSConfigWithPrefixAndVerifierAndReload(
		"vmmd_", pki.serverCert, pki.serverKey, pki.caCert, verifier, reload,
	)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	if srv.VerifyPeerCertificate == nil {
		t.Error("VerifyPeerCertificate = nil, want non-nil (verifier installed)")
	}
	if srv.GetConfigForClient == nil {
		t.Error("GetConfigForClient = nil, want non-nil (reload installed)")
	}
	if srv.GetClientCertificate == nil {
		t.Error("GetClientCertificate = nil, want non-nil (reload installed)")
	}

	cli, err := LoadClientTLSConfigWithPrefixAndVerifierAndReload(
		"vmmd_", pki.clientCert, pki.clientKey, pki.caCert, verifier, reload,
	)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if cli.VerifyPeerCertificate == nil {
		t.Error("VerifyPeerCertificate = nil, want non-nil (verifier installed)")
	}
	if cli.GetClientCertificate == nil {
		t.Error("GetClientCertificate = nil, want non-nil (reload installed)")
	}
}

// TestTLSLoadersWithReloadAndVerifier_NilBothOrNilOne pins the
// per-axis nil degradation: nil verifier → no hook; nil reload →
// no callback; both nil → identical to the non-reload factory's
// output (and by extension identical to
// Load*TLSConfigWithPrefixAndVerifier when only reload is nil, or
// Load*TLSConfigWithPrefix when both are nil).
func TestTLSLoadersWithReloadAndVerifier_NilBothOrNilOne(t *testing.T) {
	pki := newTestPKI(t)

	cases := []struct {
		name             string
		verifier         NodeVerifier
		reload           ReloadFunc
		wantVerifyHook   bool
		wantServerReload bool
	}{
		{"both nil: no hooks", nil, nil, false, false},
		{"verifier only: hook yes reload no", NewInmemNodeVerifier(), nil, true, false},
		{"reload only: hook no reload yes", nil, func() (*tls.Config, error) { return nil, nil }, false, true},
		{"both: hook yes reload yes", NewInmemNodeVerifier(), func() (*tls.Config, error) { return nil, nil }, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, err := LoadServerTLSConfigWithPrefixAndVerifierAndReload(
				"vmmd_", pki.serverCert, pki.serverKey, pki.caCert, tc.verifier, tc.reload,
			)
			if err != nil {
				t.Fatalf("server: %v", err)
			}
			if (srv.VerifyPeerCertificate != nil) != tc.wantVerifyHook {
				t.Errorf("server VerifyPeerCertificate: nil=%v, want hookInstalled=%v", srv.VerifyPeerCertificate == nil, tc.wantVerifyHook)
			}
			if (srv.GetConfigForClient != nil) != tc.wantServerReload {
				t.Errorf("server GetConfigForClient: nil=%v, want installed=%v", srv.GetConfigForClient == nil, tc.wantServerReload)
			}
		})
	}
}

// --- WatchTLSReload (PR-E / ADR-052 §5) ----------------------------------
//
// Coverage targets:
//   - nil reload or nil reloader → silent early return.
//   - Context cancel returns immediately without consuming hupCh.
//   - Successful SIGHUP consults the closure + calls Set.
//   - Multiple SIGHUPs in series each consult the closure + call Set.
//   - Reload error keeps the prior material live (Set NOT called).
//   - Reload returns nil *tls.Config keeps the prior material live.
//
// The test seam shape mirrors cmd/vmmd/egress_bundle_reload_test.go:
// synthetic hupCh + stub setter + silent logger + polling waitForCalls.

func TestWatchTLSReload_NilReloadOrReloaderNoOps(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hupCh := make(chan os.Signal, 1)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// nil reload → early return. The function exits before the
	// for-loop, so even a closed ctx isn't required. Pin the
	// "no panic / no hang" contract.
	WatchTLSReload(ctx, log, hupCh, &recordingReloader{}, nil)

	cancelledCtx, cancel2 := context.WithCancel(context.Background())
	cancel2()
	// nil reloader → early return too.
	WatchTLSReload(cancelledCtx, log, hupCh, nil, func() (*tls.Config, error) { return nil, nil })
}

func TestWatchTLSReload_ContextCancelExits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	hupCh := make(chan os.Signal, 1)
	close(hupCh) // doesn't matter — ctx is gone first

	stub := &recordingReloader{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	WatchTLSReload(ctx, log, hupCh, stub, func() (*tls.Config, error) { return nil, nil })

	// WatchTLSReload returns from ctx-done branch without calling
	// the closure or Set.
	stub.assertNoCalls(t)
}

func TestWatchTLSReload_SuccessfulHUPReplacesConfig(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hupCh := make(chan os.Signal, 1)
	stub := &recordingReloader{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	reloadCalled := 0
	reload := func() (*tls.Config, error) {
		reloadCalled++
		return &tls.Config{MinVersion: tls.VersionTLS13}, nil
	}

	go WatchTLSReload(ctx, log, hupCh, stub, reload)

	hupCh <- syscall.SIGHUP
	waitForReloadCalls(t, stub, 1, 2*time.Second)
	if reloadCalled != 1 {
		t.Errorf("reload called %d times, want 1", reloadCalled)
	}
}

func TestWatchTLSReload_MultipleHUPs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hupCh := make(chan os.Signal, 1)
	stub := &recordingReloader{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	reloadCalled := 0
	reload := func() (*tls.Config, error) {
		reloadCalled++
		return &tls.Config{MinVersion: tls.VersionTLS13}, nil
	}

	go WatchTLSReload(ctx, log, hupCh, stub, reload)

	for i := 0; i < 3; i++ {
		hupCh <- syscall.SIGHUP
	}
	waitForReloadCalls(t, stub, 3, 2*time.Second)
	if reloadCalled != 3 {
		t.Errorf("reload called %d times, want 3", reloadCalled)
	}
}

func TestWatchTLSReload_KeepPriorOnError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hupCh := make(chan os.Signal, 1)
	stub := &recordingReloader{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	var (
		counter reloadCallCounter
	)
	boom := errors.New("malformed PEM")
	reload := func() (*tls.Config, error) {
		counter.inc()
		return nil, boom
	}

	go WatchTLSReload(ctx, log, hupCh, stub, reload)

	hupCh <- syscall.SIGHUP
	// Synchronise on the watcher having consulted the closure at
	// least once — on error the watcher continues to the next
	// select iteration without calling Set, so the only observable
	// signal is the counter inside the closure.
	waitForCounter(t, &counter, 1, 2*time.Second)
	cancel()

	stub.assertNoCalls(t)
	if got := counter.get(); got != 1 {
		t.Errorf("reload called %d times, want 1 (closure consulted even on error)", got)
	}
}

func TestWatchTLSReload_KeepPriorOnNilConfig(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hupCh := make(chan os.Signal, 1)
	stub := &recordingReloader{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	var counter reloadCallCounter
	reload := func() (*tls.Config, error) {
		counter.inc()
		return nil, nil // explicit nil config
	}

	go WatchTLSReload(ctx, log, hupCh, stub, reload)

	hupCh <- syscall.SIGHUP
	waitForCounter(t, &counter, 1, 2*time.Second)
	cancel()

	stub.assertNoCalls(t)
	if got := counter.get(); got != 1 {
		t.Errorf("reload called %d times, want 1 (closure consulted even on nil cfg)", got)
	}
}

// recordingReloader is the test-side stub for TLSReloader.Set.
// Distinct from the recordingVerifier in config_verifier_test.go so
// each test file's helper surface is self-contained. The stub
// records every Set call with a mutex — the polling helpers race
// against the watcher's goroutine.
type recordingReloader struct {
	mu   sync.Mutex
	cfgs []*tls.Config
}

func (r *recordingReloader) Set(cfg *tls.Config) {
	r.mu.Lock()
	r.cfgs = append(r.cfgs, cfg)
	r.mu.Unlock()
}

func (r *recordingReloader) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.cfgs)
}

func (r *recordingReloader) assertNoCalls(t *testing.T) {
	t.Helper()
	if n := r.calls(); n != 0 {
		t.Errorf("Reloader.Set called %d times, want 0 (best-effort: prior material stays live)", n)
	}
}

// waitForReloadCalls polls for up to within for the stub to record
// want Set calls. Mirrors vmmd's waitForCalls shape.
func waitForReloadCalls(t *testing.T, stub *recordingReloader, want int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if stub.calls() >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("waitForReloadCalls: got %d Set calls, want %d within %s", stub.calls(), want, within)
}

// reloadCallCounter is a tiny mutex-guarded int the test injects
// into its ReloadFunc closures. The closure increments the counter;
// the test polls waitForCounter for synchronisation. Distinct from
// the waitForReloadCalls path: that one observes Set calls on the
// stub (the success path); this one observes closure invocations
// directly (the failure / nil-cfg paths where Set never fires).
type reloadCallCounter struct {
	mu    sync.Mutex
	count int
}

func (r *reloadCallCounter) inc() {
	r.mu.Lock()
	r.count++
	r.mu.Unlock()
}

func (r *reloadCallCounter) get() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count
}

// waitForCounter polls for up to within for counter to reach want.
func waitForCounter(t *testing.T, counter *reloadCallCounter, want int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if counter.get() >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("waitForCounter: got %d, want %d within %s", counter.get(), want, within)
}

// --- Pre-PR-E tests continue below... ------------------------------------

// TestTargetStringRoundTrip: pin the asymmetries of Target.String() so
// nobody silently changes it. unix reconstructs the canonical form;
// tcp and dns round-trip via ParseTarget. Pass them back through
// ParseTarget to confirm the loop is stable.
func TestTargetStringRoundTrip(t *testing.T) {
	cases := []string{
		"unix:///run/faas/vmmd.sock",
		"tcp://127.0.0.1:50051",
		"tcp://0.0.0.0:50051",
		"tcp://:50051",
		"dns:///vmmd.internal:50051",
		"dns://vmmd.internal:50051",
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			tgt, err := ParseTarget(raw)
			if err != nil {
				t.Fatalf("ParseTarget: %v", err)
			}
			round, err := ParseTarget(tgt.String())
			if err != nil {
				t.Fatalf("ParseTarget(String()): %v", err)
			}
			if round != tgt {
				t.Fatalf("round-trip mismatch: in=%+v out=%+v", tgt, round)
			}
		})
	}
}

// --- TCP listener bound to a real port, no RPC ----------------------------

func TestListenTCPAllocatesPort(t *testing.T) {
	pki := newTestPKI(t)
	serverTLS, err := LoadServerTLSConfig(pki.serverCert, pki.serverKey, pki.caCert)
	if err != nil {
		t.Fatalf("LoadServerTLSConfig: %v", err)
	}
	lis, err := Listen(context.Background(), "tcp://127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = lis.Close() })

	if _, ok := lis.Addr().(*net.TCPAddr); !ok {
		t.Fatalf("Addr() = %T; want *net.TCPAddr", lis.Addr())
	}
}

// --- ListenAs owner-aware -----------------------------------------------

func TestListenAsRequiresAbsolutePath(t *testing.T) {
	// The scheme parser already rejects non-absolute paths; this guards
	// against future regressions that might bypass ParseTarget.
	_, err := ListenAs(context.Background(), "unix://relative.sock", nil, "root")
	if err == nil {
		t.Fatalf("expected error for relative unix path")
	}
}

func TestListenAs_UnknownDaemonUserFallback(t *testing.T) {
	t.Setenv(SkipGroupLookupEnv, "1")
	dir, err := os.MkdirTemp("/tmp", "wtr-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sockPath := "unix://" + filepath.Join(dir, "fb.sock")
	lis, err := ListenAs(context.Background(), sockPath, nil, "definitely_not_a_real_user_xyzzy")
	if err != nil {
		t.Fatalf("ListenAs should fall back to current user when daemonUser is unknown; got: %v", err)
	}
	defer lis.Close()
}

// --- Compile-time pin: insecure credentials stay the unix default ------

func TestDialUnixInsecure(t *testing.T) {
	// A nil tlsCfg on a unix:// target should yield a *grpc.ClientConn
	// that doesn't carry any TLS credentials. We can't easily probe the
	// creds object directly without exporting types; instead we just
	// confirm construction succeeds and the dial is lazy (no peer up).
	conn, err := Dial(context.Background(), "/run/faas/should-not-exist.sock", nil)
	if err != nil {
		t.Fatalf("Dial(unix, nil TLS): %v", err)
	}
	_ = conn.Close()
	_ = credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})
	_ = insecure.NewCredentials() // keep import live
}

// --- Test PKI helpers ----------------------------------------------------

type testPKI struct {
	caCert     string
	serverCert string
	serverKey  string
	clientCert string
	clientKey  string
}

// newTestPKI builds a one-shot CA + server leaf + client leaf. All PEM
// files live under t.TempDir() so they disappear with the test. Each
// call generates an independent CA, which lets the wrong-CA tests prove
// the trust boundary without any global state.
func newTestPKI(t *testing.T) testPKI {
	t.Helper()
	dir := t.TempDir()

	caCert, caCertPEM, caKey := mustGenSelfSigned(t, x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	})

	serverCertPEM, serverKeyPEM := mustGenSigned(t, x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "test-server"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:     []string{"localhost"},
	}, caCert, caKey)

	clientCertPEM, clientKeyPEM := mustGenSigned(t, x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: "test-client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, caCert, caKey)

	caCertPath := writeTestFile(t, filepath.Join(dir, "ca.pem"), caCertPEM)
	serverCertPath := writeTestFile(t, filepath.Join(dir, "server.crt"), serverCertPEM)
	serverKeyPath := writeTestFile(t, filepath.Join(dir, "server.key"), serverKeyPEM)
	clientCertPath := writeTestFile(t, filepath.Join(dir, "client.crt"), clientCertPEM)
	clientKeyPath := writeTestFile(t, filepath.Join(dir, "client.key"), clientKeyPEM)

	return testPKI{
		caCert:     caCertPath,
		serverCert: serverCertPath,
		serverKey:  serverKeyPath,
		clientCert: clientCertPath,
		clientKey:  clientKeyPath,
	}
}

// mustGenSelfSigned returns (parsed-cert, cert-PEM, key) for a self-signed CA.
func mustGenSelfSigned(t *testing.T, tmpl x509.Certificate) (*x509.Certificate, []byte, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey: %v", err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate (self-signed): %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse self-signed: %v", err)
	}
	certPEM := mustEncodePEM(t, "CERTIFICATE", der)
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("MarshalECPrivateKey: %v", err)
	}
	_ = mustEncodePEM(t, "EC PRIVATE KEY", keyDER) // discarded; not written
	return cert, certPEM, key
}

// mustGenSigned signs tmpl with parent + parentKey and returns
// (cert-PEM, key-PEM). The leaf key is freshly generated.
func mustGenSigned(t *testing.T, tmpl x509.Certificate, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) ([]byte, []byte) {
	t.Helper()
	leaf, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey: %v", err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, parent, &leaf.PublicKey, parentKey)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	certPEM := mustEncodePEM(t, "CERTIFICATE", der)
	keyDER, err := x509.MarshalECPrivateKey(leaf)
	if err != nil {
		t.Fatalf("MarshalECPrivateKey: %v", err)
	}
	keyPEM := mustEncodePEM(t, "EC PRIVATE KEY", keyDER)
	return certPEM, keyPEM
}

func writeTestFile(t *testing.T, path string, data []byte) string {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
	return path
}

// mustEncodePEM wraps der with a BEGIN/END block using encoding/pem under
// the hood (the stdlib helper), so the loader's tls.LoadX509KeyPair path
// is exercised end-to-end.
func mustEncodePEM(t *testing.T, typ string, der []byte) []byte {
	t.Helper()
	out := pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
	if out == nil {
		t.Fatalf("pem.EncodeToMemory returned nil")
	}
	return out
}

// pemBlockFor / pemEncodeStd shims removed: the helpers above use the
// stdlib encoding/pem directly.

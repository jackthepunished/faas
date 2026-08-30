// Bridge H2C terminator e2e tests (ADR-126 / G19). These spin up
// the real vmmd-stream-bridge binary, wire it to a local H2C "guest"
// listener (the same shape as a guest's :8080), and drive inbound
// H2C requests through it. The test is the load-bearing pin that:
//   - the bridge binary, when FAAS_BRIDGE_PROTOCOL=h2c, terminates
//     H2C end-to-end (inbound H2C → bridge → guest H2C)
//   - when FAAS_BRIDGE_PROTOCOL=h1 (default), falls back to the
//     legacy H1+chunked path (regression: app_protocol=http1 stays
//     verbatim)
//   - trailers round-trip verbatim for the grpc code path
//     (load-bearing invariant for grpc trailers)
//
// These tests do not require KVM / Firecracker / the platform's
// apid — they exercise the bridge binary as a stand-alone process
// (it's `go build ./cmd/vmmd-stream-bridge` then exec'd against a
// loopback guest listener). Metal-grade end-to-end against a real
// Firecracker guest is in bridge_h2c_terminator_metal_test.go
// (//go:build metal).
//
// CI-safe (no build tag). Skips if the bridge binary isn't
// buildable on this machine.

package e2e_test

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// bridgeBinary builds vmmd-stream-bridge into a temp dir and
// returns its absolute path. Skips the test (via t.Skip) if the
// build fails — typically because the host lacks a toolchain or
// the repo's go.mod points at unrecoverable state.
func bridgeBinary(t *testing.T) string {
	t.Helper()
	tmp, err := os.MkdirTemp("", "bridge-h2c-e2e-")
	if err != nil {
		t.Skipf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	bin := filepath.Join(tmp, "vmmd-stream-bridge")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/vmmd-stream-bridge")
	root := repoRoot()
	if root == "" {
		t.Skip("module root not reachable — run from the repo root or fix cwd")
	}
	cmd.Dir = root
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Skipf("bridge build failed: %v", err)
	}
	return bin
}

// spawnBridge launches the bridge binary with the given framing
// env var, listening on a unix socket, terminating toward
// guestAddr:guestPort. Returns the unix socket path; the caller
// drives requests against it.
func spawnBridge(t *testing.T, bin, framing, sockPath, guestAddr string, guestPort uint16) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(bin, sockPath, guestAddr, fmt.Sprintf("%d", guestPort), "30s")
	root := repoRoot()
	if root == "" {
		t.Skip("module root not reachable — run from the repo root or fix cwd")
	}
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"FAAS_BRIDGE_PROTOCOL="+framing,
	)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("bridge start: %v", err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-waitCh:
		case <-time.After(2 * time.Second):
			_ = cmd.Process.Kill()
			<-waitCh
		}
	})
	// Wait for the unix socket to appear. The bridge itself starts quickly,
	// but this package is run alongside every other package in `go test ./...`
	// and the child may be CPU-starved while the host builds/tests those
	// packages. Keep the startup budget bounded without making the E2E test
	// fail spuriously under repository-wide load.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			return cmd
		}
		select {
		case err := <-waitCh:
			t.Fatalf("bridge exited before binding socket (err=%v)", err)
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("bridge socket %s never appeared after 10s", sockPath)
	return nil
}

// bridgeRoundTripper returns an http2.Transport over the bridge's
// unix socket. Mirrors pkg/vmmdgrpc/forward.go::newStreamBridgeH2CTransport.
func bridgeRoundTripper(sockPath string) *http2.Transport {
	return &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, _, _ string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sockPath)
		},
		IdleConnTimeout: 5 * time.Second,
		ReadIdleTimeout: 1 * time.Second,
		PingTimeout:     100 * time.Millisecond,
	}
}

// TestBridgeH2CTerminator_H2CUnaryRequest pins the G19 closure for
// app_protocol=http2: inbound H2C request reaches the guest's H2C
// listener verbatim. Asserts the body bytes, headers, and
// response status round-trip end-to-end.
func TestBridgeH2CTerminator_H2CUnaryRequest(t *testing.T) {
	bin := bridgeBinary(t)

	// Local H2C "guest" — the same shape as a runner's :8080 listener
	// (srv.Protocols.SetUnencryptedHTTP2(true) per ADR-126 / G19).
	guestMux := http.NewServeMux()
	guestMux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Guest", "ok")
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "guest saw method=%s path=%s body=%s", r.Method, r.URL.Path, body)
	})
	guestSrv := &http.Server{Handler: guestMux}
	guestSrv.Protocols = new(http.Protocols)
	guestSrv.Protocols.SetHTTP1(true)
	guestSrv.Protocols.SetUnencryptedHTTP2(true)
	guestLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("guest listen: %v", err)
	}
	t.Cleanup(func() { _ = guestLn.Close() })
	go func() { _ = guestSrv.Serve(guestLn) }()

	sockPath := filepath.Join("/tmp", fmt.Sprintf("bridge-%d.sock", time.Now().UnixNano()))
	t.Cleanup(func() { _ = os.Remove(sockPath) })
	spawnBridge(t, bin, "h2c", sockPath, "127.0.0.1", uint16(guestLn.Addr().(*net.TCPAddr).Port))
	rt := bridgeRoundTripper(sockPath)
	t.Cleanup(func() { rt.CloseIdleConnections() })

	req, err := http.NewRequest("POST", "http://bridge.invalid/echo", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	req.Header.Set("X-Custom", "test-value")
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("bridge roundtrip: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Guest"); got != "ok" {
		t.Errorf("X-Guest header = %q, want %q", got, "ok")
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "guest saw method=POST path=/echo body=hello" {
		t.Errorf("body = %q", body)
	}
}

// TestBridgeH2CTerminator_H1Fallback pins the regression for
// app_protocol=http1: when FAAS_BRIDGE_PROTOCOL=h1, the bridge
// falls back to the legacy H1+chunked path. The bridge's inbound
// side still speaks H2C (per the v2 cutover, PR #750), but the
// outbound side writes H1+chunked against the guest. The guest
// listener accepts H1 (srv.Protocols.SetHTTP1(true) + the default).
func TestBridgeH2CTerminator_H1Fallback(t *testing.T) {
	bin := bridgeBinary(t)

	// Plain H1 guest listener (no H2 opt-in required).
	// The bridge's H1 path uses env-driven FAAS_BRIDGE_URL (default
	// "/" when no env is set), so the guest must register "/" — not
	// "/h1" — to receive the bridge's outbound request.
	guestMux := http.NewServeMux()
	guestMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "h1 OK")
	})
	guestSrv := &http.Server{Handler: guestMux}
	guestLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("guest listen: %v", err)
	}
	t.Cleanup(func() { _ = guestLn.Close() })
	go func() { _ = guestSrv.Serve(guestLn) }()

	sockPath := filepath.Join("/tmp", fmt.Sprintf("bridge-h1-%d.sock", time.Now().UnixNano()))
	t.Cleanup(func() { _ = os.Remove(sockPath) })
	spawnBridge(t, bin, "h1", sockPath, "127.0.0.1", uint16(guestLn.Addr().(*net.TCPAddr).Port))
	rt := bridgeRoundTripper(sockPath)
	t.Cleanup(func() { rt.CloseIdleConnections() })

	resp, err := rt.RoundTrip(&http.Request{
		Method: "GET",
		URL:    mustURL("http://bridge.invalid/h1"),
		Header: make(http.Header),
	})
	if err != nil {
		t.Fatalf("bridge roundtrip: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "h1 OK" {
		t.Errorf("body = %q, want %q", body, "h1 OK")
	}
}

// TestBridgeH2CTerminator_GRPCTrailers pins the load-bearing gRPC
// invariant (ADR-126 §Decision 5): trailer HEADERS frames round-trip
// from guest to inbound caller verbatim.
func TestBridgeH2CTerminator_GRPCTrailers(t *testing.T) {
	bin := bridgeBinary(t)

	guestMux := http.NewServeMux()
	guestMux.HandleFunc("/grpc", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Trailer", "Grpc-Status, Grpc-Message")
		w.Header().Set(http.TrailerPrefix+"Grpc-Status", "")
		w.Header().Set(http.TrailerPrefix+"Grpc-Message", "")
		w.Header().Set("Content-Type", "application/grpc")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "grpc body")
		w.Header().Set(http.TrailerPrefix+"Grpc-Status", "0")
		w.Header().Set(http.TrailerPrefix+"Grpc-Message", "")
	})
	guestSrv := &http.Server{Handler: guestMux}
	guestSrv.Protocols = new(http.Protocols)
	guestSrv.Protocols.SetHTTP1(true)
	guestSrv.Protocols.SetUnencryptedHTTP2(true)
	guestLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("guest listen: %v", err)
	}
	t.Cleanup(func() { _ = guestLn.Close() })
	go func() { _ = guestSrv.Serve(guestLn) }()

	sockPath := filepath.Join("/tmp", fmt.Sprintf("bridge-grpc-%d.sock", time.Now().UnixNano()))
	t.Cleanup(func() { _ = os.Remove(sockPath) })
	spawnBridge(t, bin, "h2c", sockPath, "127.0.0.1", uint16(guestLn.Addr().(*net.TCPAddr).Port))
	rt := bridgeRoundTripper(sockPath)
	t.Cleanup(func() { rt.CloseIdleConnections() })

	req, err := http.NewRequest("POST", "http://bridge.invalid/grpc", nil)
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	req.Header.Set("Content-Type", "application/grpc")
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("bridge roundtrip: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	_ = body // drain
	if got := resp.Trailer.Get("Grpc-Status"); got != "0" {
		t.Errorf("Grpc-Status trailer = %q, want %q (load-bearing invariant for grpc)", got, "0")
	}
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

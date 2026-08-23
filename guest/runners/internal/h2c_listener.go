// Package internal — runner helpers shared across guest/runners/{go124,
// node22, node24, python312, python313}. The H2C-capable listener is
// the entrypoint shared by all runners; it must opt into HTTP/2
// prior-knowledge so the bridge's H2C terminator (ADR-126, G19) can
// speak native H2 to the customer's :8080 listener when
// app_protocol ∈ {http2, grpc}.
//
// Why a shared helper:
//   - One opt-in site per runner (DRY) keeps the closed-set
//     (app_protocol ∈ {http1, http2, grpc}) consistent across runtimes.
//   - Go 1.24+ stdlib `srv.Protocols.SetUnencryptedHTTP2(true)` is
//     the canonical replacement for the deprecated
//     `golang.org/x/net/http2/h2c.NewHandler` wrapper. The runtime
//     needs Go 1.24+ — see images/runner-go124.Dockerfile + the
//     images/runner-go124-alpine.Dockerfile. Older runtimes (Node 22,
//     Python 3.12/3.13) are H1-only and fall back to the legacy
//     H1+chunked bridge path on the wire; their customers keep
//     working unchanged because app_protocol=http1 stays on the
//     legacy path forever (ADR-126 §Decision 6).
//   - The listener helper does NOT change wire behavior for an
//     H1-only caller (the stdlib server selects HTTP/1.1 by default
//     when the client doesn't open an H2 prior-knowledge preface).
package internal

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// H2CListenerConfig is the runner-side configuration for the
// per-runtime H2C-capable :8080 listener. Keep it small — every
// field is load-bearing and every new one should come with a
// rationale referencing the spec / ADR that drove it.
type H2CListenerConfig struct {
	// Addr is the bind address inside the guest netns (always
	// ":8080" via DefaultAppPort in production; PORT env override
	// for tests).
	Addr string

	// Handler is the per-runtime ServeMux (the runtime's specific
	// /healthz + handler envelope).
	Handler http.Handler

	// ReadHeaderTimeout caps the H2C connection-preface read;
	// 10s is generous on loopback (liveness probes fire every 1s).
	ReadHeaderTimeout time.Duration

	// ReadTimeout caps an idle connection read; 0 = no limit
	// (matches vmmd-stream-bridge's headless H2C path; the
	// bridge ctx deadline + the runtime's tail host cap the
	// stream length end-to-end).
	ReadTimeout time.Duration

	// WriteTimeout caps an idle write; 0 = no limit (matches
	// the bridge).
	WriteTimeout time.Duration
}

// DefaultH2CListenerConfig is the canonical runner config —
// port from PORT env (matches the runner's prior call shape;
// empty falls back to ":8080"), 10s header timeout, no
// per-call read/write limits.
func DefaultH2CListenerConfig(handler http.Handler) H2CListenerConfig {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	return H2CListenerConfig{
		Addr:              ":" + port,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
}

// ListenAndServeH2C binds the H2C-capable listener on cfg.Addr and
// serves until SIGTERM/SIGINT or a fatal serve error. The HTTP/2
// prior-knowledge opt-in is the load-bearing G19 / ADR-126 line:
// without it, the listener is H1-only and the bridge's H2C
// terminator can't speak native H2 to the guest.
//
// Three rejection shapes:
//  1. bind failure (port already in use, no netns) → log.Fatal
//  2. serve failure (kernel TCP error) → log.Fatal
//  3. SIGTERM / SIGINT → graceful shutdown, exit 0
//
// Signal handling mirrors the bridge's SIGTERM/SIGINT graceful
// shutdown (cmd/vmmd-stream-bridge/main.go:165) so the runner
// shuts down cleanly when guest-init tears down the guest.
func ListenAndServeH2C(cfg H2CListenerConfig) {
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           cfg.Handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
	}
	// H2C opt-in (ADR-126 §Decision 1, G19). Replaces the
	// deprecated golang.org/x/net/http2/h2c.NewHandler wrapper.
	// The Protocols struct starts with NO protocols set (a
	// zero-value Protocols has HTTP1() == false), so we must
	// explicitly SetHTTP1(true) AND SetUnencryptedHTTP2(true) —
	// otherwise H1 callers (app_protocol=http1) get a connection
	// reset, which is the load-bearing backwards-compat regression
	// we're protecting against.
	srv.Protocols = new(http.Protocols)
	srv.Protocols.SetHTTP1(true)
	srv.Protocols.SetUnencryptedHTTP2(true)

	// SIGTERM / SIGINT → graceful shutdown. guest-init sends
	// SIGTERM after the wake's framework_ready signal fires;
	// the runner exits cleanly without truncating an in-flight
	// envelope.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	errc := make(chan error, 1)
	go func() {
		log.Printf("h2c-listener: listening on %s (H2C-capable)", cfg.Addr)
		err := srv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
			return
		}
		errc <- nil
	}()

	select {
	case <-ctx.Done():
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutCancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			log.Printf("h2c-listener: shutdown: %v", err)
		}
		<-errc
	case err := <-errc:
		if err != nil {
			log.Fatalf("h2c-listener: serve: %v", err)
		}
	}
}

// ListenAndServeLoopback is a test-only convenience that returns
// the listener (already bound) instead of blocking the goroutine.
// Used by the runner unit tests to assert H2C framing on a local
// port. Production runners use ListenAndServeH2C.
//
// The returned *http.Server is configured identically to the
// production code path (H2C-capable, ReadHeaderTimeout applied)
// so the test exercises the same listener contract.
func ListenAndServeLoopback(cfg H2CListenerConfig) (*http.Server, net.Listener, error) {
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           cfg.Handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
	}
	srv.Protocols = new(http.Protocols)
	srv.Protocols.SetHTTP1(true)
	srv.Protocols.SetUnencryptedHTTP2(true)
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, nil, fmt.Errorf("listen %s: %w", cfg.Addr, err)
	}
	return srv, ln, nil
}
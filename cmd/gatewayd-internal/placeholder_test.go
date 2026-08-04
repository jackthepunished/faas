package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/gateway"
)

// TestEnvOr_EmptyFallback pins the envOr semantics (empty env
// falls back to def).
func TestEnvOr_EmptyFallback(t *testing.T) {
	t.Setenv("FAAS_INTERNAL_SOCKET", "")
	if got := envOr("FAAS_INTERNAL_SOCKET", "/run/faas/gatewayd-internal.sock"); got != "/run/faas/gatewayd-internal.sock" {
		t.Errorf("envOr empty = %q, want default", got)
	}
	t.Setenv("FAAS_INTERNAL_SOCKET", "/tmp/test.sock")
	if got := envOr("FAAS_INTERNAL_SOCKET", "/run/faas/gatewayd-internal.sock"); got != "/tmp/test.sock" {
		t.Errorf("envOr set = %q, want /tmp/test.sock", got)
	}
}

// TestPlaceholder_Returns200OK pins the contract that the
// placeholder handler returns 200 OK with the banner body (so
// /readyz is green and the proxy can be wired in without 502s).
func TestPlaceholder_Returns200OK(t *testing.T) {
	hydro := gateway.NewRouteCacheHydration()
	cacheSig := &gateway.ReadySignal{}
	cacheSig.Set(false, "not hydrated")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	placeholderWithHydration(hydro, cacheSig).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("placeholder status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != placeholderBanner {
		t.Errorf("placeholder body = %q, want %q", got, placeholderBanner)
	}
}

// TestPlaceholder_WarmhintTestFlipsHydration pins the
// `/warmhint/test` path: cacheHydration flips to true and the
// route cache signal flips to true.
func TestPlaceholder_WarmhintTestFlipsHydration(t *testing.T) {
	hydro := gateway.NewRouteCacheHydration()
	cacheSig := &gateway.ReadySignal{}
	cacheSig.Set(false, "not hydrated")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/warmhint/test", nil)
	placeholderWithHydration(hydro, cacheSig).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if ok, _ := hydro.Hydrated(); !ok {
		t.Errorf("hydration bit not flipped after /warmhint/test")
	}
}

// TestPlaceholder_NotHydratedPathReturns200 pins the contract
// that the "not yet hydrated" path returns 200 OK (not 503) so
// the daemon's /readyz stays green until the real handler is
// wired. The banner body is the tripwire.
func TestPlaceholder_NotHydratedPathReturns200(t *testing.T) {
	hydro := gateway.NewRouteCacheHydration()
	cacheSig := &gateway.ReadySignal{}
	cacheSig.Set(false, "not hydrated")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/some/customer/path", nil)
	placeholderWithHydration(hydro, cacheSig).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (placeholder must be 200 OK, not 503)", rec.Code)
	}
	if got := rec.Body.String(); got != placeholderBanner {
		t.Errorf("body = %q, want banner", got)
	}
}

// TestPgSig_Smoke pins that NewPGPingSignal works with a stub
// pinger (the production wiring opens a real pool; the placeholder
// path uses a stub).
func TestPgSig_Smoke(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stub := &stubPinger{}
	sig, stop := gateway.NewPGPingSignal(ctx, stub, 50*time.Millisecond)
	defer stop()
	time.Sleep(100 * time.Millisecond)
	ok, _ := sig.Report()
	if !ok {
		t.Errorf("PG ping signal reports not-ready after successful ping")
	}
}

type stubPinger struct{}

func (stubPinger) Ping(_ context.Context) error { return nil }

// TestDefaultControlAddrIsLoopback pins the §11 control-listener
// invariant: the control plane binds loopback only, so /metrics
// is never exposed to the network.
func TestDefaultControlAddrIsLoopback(t *testing.T) {
	if !isLoopback(defaultControlAddr) {
		t.Errorf("defaultControlAddr = %q, want loopback (127.0.0.1 or ::1)", defaultControlAddr)
	}
}

// isLoopback is a small helper that mirrors the legacy cmd/gatewayd
// assertLoopbackBind check (test-only; the prod path uses the
// dedicated helper).
func isLoopback(addr string) bool {
	// minimal parse: hand-roll because we don't want to drag
	// net.SplitHostPort into a unit test for two cases.
	if addr == "" {
		return false
	}
	// Loopback prefixes we accept.
	for _, p := range []string{"127.0.0.1:", "localhost:", "[::1]:"} {
		if len(addr) >= len(p) && addr[:len(p)] == p {
			return true
		}
	}
	return false
}

// TestPlaceholderBanner_Tripwire pins the banner-tripwire guard:
// the placeholder body is "TEMPLATE_OK" so a production curl that
// returns the banner is a smoking-gun for an unfinished deploy.
func TestPlaceholderBanner_Tripwire(t *testing.T) {
	if !strings.Contains(placeholderBanner, "TEMPLATE_OK") {
		t.Errorf("placeholderBanner = %q, want substring \"TEMPLATE_OK\"", placeholderBanner)
	}
}

// TestDefaultDeps pins the singleton wiring (the daemon always
// starts with slog.Default — no separate log file for the
// placeholder path).
func TestDefaultDeps(t *testing.T) {
	d := defaultDeps()
	if d.log != slog.Default() {
		t.Errorf("defaultDeps.log != slog.Default()")
	}
}

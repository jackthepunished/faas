package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/gateway"
)

// fixedBackend is a Backend that returns whatever the test sets. Used to
// exercise the handler composition without depending on the unwired default.
// Issue #168: shaped to the new Backend interface (Pick / Admit / HealthyCount).
type fixedBackend struct {
	app        gateway.App
	appOK      bool
	picks      []gateway.Target
	pickIdx    int
	admitErr   error
	admitCalls int
	atCap      bool
}

func (f *fixedBackend) Lookup(_ context.Context, _ string) (gateway.App, bool) {
	return f.app, f.appOK
}
func (f *fixedBackend) Pick(_ string) (gateway.Target, bool) {
	if len(f.picks) == 0 {
		return gateway.Target{}, false
	}
	t := f.picks[f.pickIdx%len(f.picks)]
	f.pickIdx++
	return t, true
}
func (f *fixedBackend) HealthyCount(_ string) int {
	return len(f.picks)
}
func (f *fixedBackend) Admit(_ context.Context, _ string, _ int) (string, gateway.WakeMethod, bool, error) {
	f.admitCalls++
	if f.admitErr != nil {
		return "", gateway.WakeMethodUnspecified, false, f.admitErr
	}
	if f.atCap {
		return "", gateway.WakeMethodUnspecified, true, nil
	}
	return "wake-fixed", gateway.WakeMethodColdBoot, false, nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestUnwiredBackendReturnsNotFound(t *testing.T) {
	b := unwiredBackend{}
	if _, ok := b.Lookup(context.Background(), "any"); ok {
		t.Error("Lookup should report not-found")
	}
	if _, ok := b.Pick("any"); ok {
		t.Error("Pick should report not-found")
	}
	if got := b.HealthyCount("any"); got != 0 {
		t.Errorf("HealthyCount = %d, want 0", got)
	}
	if _, _, _, err := b.Admit(context.Background(), "any", 1); err != nil {
		t.Errorf("Admit should be no-op: %v", err)
	}
}

func TestRunWithDeps_ServesAndShutsDown(t *testing.T) {
	deps := prodDefaultDeps()
	deps.backend = &fixedBackend{}
	deps.newSrv = func(addr string, h http.Handler) *http.Server {
		return &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second}
	}
	// Bind a real listener up front and pass it in via a closure-captured
	// pointer so we can read its address synchronously.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deps.listen = func(_, _ string) (net.Listener, error) { return ln, nil }
	// Free-port the control listener so this test doesn't race with
	// TestRunWithDeps_TLSBundleCloseStopsRenewLoop for the hard-coded
	// 127.0.0.1:9090.
	ctrlLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = ctrlLn.Close()
	deps.controlAddr = ctrlLn.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- prodRunWithDeps(ctx, discardLogger(), deps) }()
	t.Cleanup(cancel)

	// Wait until the server is accepting.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Hit the server — it should 404 since fixedBackend's Lookup/Target
	// return not-found.
	resp, err := http.Get("http://" + ln.Addr().String() + "/anything")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "Server closed") && !strings.Contains(err.Error(), "use of closed network connection") {
			t.Errorf("prodRunWithDeps returned %v, want nil or context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("prodRunWithDeps did not return after ctx cancel")
	}
}

func TestRunWithDeps_ListenErrorReturns(t *testing.T) {
	deps := prodDefaultDeps()
	deps.listen = func(_, _ string) (net.Listener, error) {
		return nil, errors.New("addr in use")
	}
	err := prodRunWithDeps(context.Background(), discardLogger(), deps)
	if err == nil {
		t.Fatal("expected listen error to propagate")
	}
	if !strings.Contains(err.Error(), "addr in use") {
		t.Errorf("error %q missing 'addr in use'", err.Error())
	}
}

func TestRunWithDeps_ServeError(t *testing.T) {
	// Use a listener we close immediately, then have the server try to Serve
	// on it. The close races with Serve so we observe either an immediate
	// Serve error or a successful Shutdown — both are acceptable termination
	// signals.
	deps := prodDefaultDeps()
	deps.backend = &fixedBackend{}

	deps.listen = func(_, _ string) (net.Listener, error) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		_ = l.Close()
		return l, nil
	}
	deps.newSrv = func(addr string, h http.Handler) *http.Server {
		return &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second}
	}

	done := make(chan error, 1)
	go func() { done <- prodRunWithDeps(context.Background(), discardLogger(), deps) }()

	select {
	case err := <-done:
		// The Serve of a closed listener returns a net error; we just want
		// the goroutine to exit cleanly. Acceptable: any non-nil OR nil.
		_ = err
	case <-time.After(2 * time.Second):
		t.Fatal("prodRunWithDeps did not exit after listener closed")
	}
}

func TestDefaultDeps_ReturnExpected(t *testing.T) {
	d := prodDefaultDeps()
	if d.listen == nil {
		t.Error("prodDefaultDeps().listen is nil")
	}
	if d.newSrv == nil {
		t.Error("prodDefaultDeps().newSrv is nil")
	}
	if d.backend == nil {
		t.Error("prodDefaultDeps().backend is nil")
	}
	if _, ok := d.backend.(unwiredBackend); !ok {
		t.Errorf("default backend = %T, want unwiredBackend", d.backend)
	}
	srv := d.newSrv(":0", http.NewServeMux())
	if srv.ReadHeaderTimeout == 0 {
		t.Error("default server should set ReadHeaderTimeout")
	}
}

func TestFixedBackend_Delegates(t *testing.T) {
	b := &fixedBackend{
		app:      gateway.App{ID: "a1", Plan: api.PlanHobby},
		appOK:    true,
		picks:    []gateway.Target{{NodeID: "10.0.0.2:8080", InstanceID: "i-1"}},
		admitErr: errors.New("upstream"),
	}
	if a, ok := b.Lookup(context.Background(), "name"); !ok || a.ID != "a1" {
		t.Errorf("Lookup = %+v,%v", a, ok)
	}
	if tgt, ok := b.Pick("a"); !ok || tgt.NodeID != "10.0.0.2:8080" {
		t.Errorf("Pick = %+v,%v", tgt, ok)
	}
	if got := b.HealthyCount("a"); got != 1 {
		t.Errorf("HealthyCount = %d, want 1", got)
	}
	if _, _, _, err := b.Admit(context.Background(), "x", 1); err == nil || err.Error() != "upstream" {
		t.Errorf("Admit err = %v", err)
	}
	if b.admitCalls != 1 {
		t.Errorf("Admit call not recorded: %d", b.admitCalls)
	}
}

// TestAssertLoopbackBind exercises the /metrics listener guard added
// in PR #218. Accepts every loopback form; rejects public addresses and
// bare ":port" (which would bind 0.0.0.0). The harness path passes a
// loopback form so the in-process tests in this file keep passing.
func TestAssertLoopbackBind(t *testing.T) {
	cases := []struct {
		addr string
		ok   bool
	}{
		// Accepted: explicit loopback forms the harness / production use.
		{"127.0.0.1:9090", true},
		{"127.0.0.42:9100", true},
		{"[::1]:9090", true},
		{"localhost:9090", true},

		// Rejected: any address that would expose /metrics off-box.
		{"0.0.0.0:9090", false},
		{":9090", false}, // bare ":port" binds 0.0.0.0 — exactly what this guard prevents
		{"10.0.0.1:9090", false},
		{"[2001:db8::1]:9090", false},

		// Rejected: malformed.
		{"no-port", false},
	}
	for _, tc := range cases {
		t.Run(tc.addr, func(t *testing.T) {
			err := assertLoopbackBind(tc.addr)
			if tc.ok && err != nil {
				t.Errorf("assertLoopbackBind(%q) = %v, want nil", tc.addr, err)
			}
			if !tc.ok && err == nil {
				t.Errorf("assertLoopbackBind(%q) = nil, want error", tc.addr)
			}
		})
	}
}

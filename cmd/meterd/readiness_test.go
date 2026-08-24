// Tests for cmd/meterd/readiness.go (issue #571 PR-A2).
//
// Loop.Health dereferences loop.cfg (SampleInterval etc.),
// so a zero-value *meter.Loop with nil cfg panics. Tests
// here use BuildReadinessProbe(nil) for the negative path and
// skip the end-to-end positive path — the production wiring
// passes a fully-initialized loop built via meter.NewLoop, and
// loop.Health's contract is pinned by pkg/meter's own tests.

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/wire"
)

func TestBuildReadinessProbe_NilLoop(t *testing.T) {
	// Nil loop (test path / config error) surfaces "meterd
	// loop nil" as the failing reason.
	p, stop := BuildReadinessProbe(nil)
	defer stop()
	if p == nil {
		t.Fatal("BuildReadinessProbe(nil) returned nil probe")
	}
	// Wait for the first adapter tick to evaluate.
	time.Sleep(50 * time.Millisecond)
	r, reason := p.All()
	if r {
		t.Errorf("nil loop: All() = true, want false (reason=%q)", reason)
	}
	if !strings.Contains(reason, "loop nil") {
		t.Errorf("reason = %q, want contains \"loop nil\"", reason)
	}
}

func TestBuildReadinessProbe_StopFlipsFalse(t *testing.T) {
	// stop() flips the signal to "meterd stopping" regardless
	// of the underlying loop state.
	p, stop := BuildReadinessProbe(nil)
	stop()
	r, reason := p.All()
	if r {
		t.Errorf("after stop: All() = true, want false")
	}
	if !strings.Contains(reason, "stopping") {
		t.Errorf("reason = %q, want contains \"stopping\"", reason)
	}
}

func TestBuildReadinessProbe_EndToEndViaControlMuxLite_NilLoop(t *testing.T) {
	// Wire the probe through ControlMuxLite with a nil loop
	// (the panic-prone path). The /readyz body must surface
	// the failing reason with the canonical "not-ready:"
	// prefix.
	p, stop := BuildReadinessProbe(nil)
	defer stop()
	time.Sleep(50 * time.Millisecond)
	mux := http.NewServeMux()
	wire.ControlMuxLite(mux, p.ReadyFunc(), p.ReasonFunc())

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("/readyz code = %d, want 503 (loop nil)", rr.Code)
	}
	body := rr.Body.String()
	if !strings.HasPrefix(body, "not-ready:") {
		t.Errorf("/readyz body = %q, want prefix \"not-ready:\"", body)
	}
	if !strings.Contains(body, "loop nil") {
		t.Errorf("/readyz body = %q, want contains \"loop nil\"", body)
	}
}

func TestBuildReadinessProbe_EndToStopOnShutdown(t *testing.T) {
	// After stop(), /readyz must return 503 with reason
	// "meterd stopping" so the §12 "Fleet readiness" panel
	// surfaces the drain window.
	p, stop := BuildReadinessProbe(nil)
	stop()
	mux := http.NewServeMux()
	wire.ControlMuxLite(mux, p.ReadyFunc(), p.ReasonFunc())
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("/readyz code = %d, want 503 (stopping)", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "stopping") {
		t.Errorf("/readyz body = %q, want contains \"stopping\"", rr.Body.String())
	}
}
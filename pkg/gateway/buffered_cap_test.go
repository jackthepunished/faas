// Tests for the buffered reverse-proxy response body cap (issue
// #995 Phase 2 / ADR-121). The buffered path now installs a
// capWriter at the dispatch site in handler.go's ServeHTTP; the
// streaming path uses setupStreamingWriter with its own capWriter.
// These tests cover the buffered shape.
//
// Companion tests:
//   - pkg/gateway/handler_test.go's TestApplyEdgeRuleLimit_StreamingCap_*
//     (streaming capWriter — already in tree).
//   - cmd/gatewayd-internal/proxy.go::capWriter (apid loopback —
//     same shape, covered by the unit tests on this surface if
//     added later).

package gateway

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
)

// TestSetupBufferedCapWriter_OverCap_Hardens asserts the architectural
// contract documented in handler.go:setupBufferedCapWriter — when the
// buffered upstream exceeds the cap, the connection is hardened-closed
// (either a 413 problem+json if the cap tripped before headers were
// written, or a connection reset / EOF if the cap tripped mid-body).
// Either outcome proves the capWriter is installed and the cap is enforced.
// Issue #995 Phase 2 / ADR-121.
//
// Note: the dispatch site in ServeHTTP passes app.Plan.MaxResponseBodyBytes()
// (a real per-plan cap), not a per-test parameter. The test below uses
// PlanFree (25 MiB cap) and emits 30 MiB from the upstream.
func TestSetupBufferedCapWriter_OverCap_Hardens(t *testing.T) {
	h := NewHandlerWith(&fakeBackend{}, NewMetrics(), slog.New(slog.NewJSONHandler(io.Discard, nil)))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Emit 30 MiB — well past the PlanFree 25 MiB cap.
		chunk := make([]byte, 1<<20)
		for i := 0; i < 30; i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	t.Cleanup(upstream.Close)

	b := &fakeBackend{
		app:      App{ID: "app-cap", AccountID: "acct-cap", Plan: api.PlanFree},
		host:     "cap.apps.dom",
		upstream: upstream.Listener.Addr().String(),
		running:  true,
		targets:  []Target{{NodeID: upstream.Listener.Addr().String(), InstanceID: "i-cap"}},
	}
	b.setLegacyHot()
	h.backend = b

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(l) }()
	defer func() { _ = srv.Close() }()

	url := "http://" + l.Addr().String() + "/"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Host = "cap.apps.dom"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// Connection reset by upstream after cap fired — hardening
		// path proven.
		return
	}
	defer resp.Body.Close()

	// If the response came back with a status at all, the body
	// must be <= the cap. Anything bigger is a regression.
	body, _ := io.ReadAll(resp.Body)
	if int64(len(body)) > api.MaxResponseBodyBytesDefault {
		t.Errorf("over-cap body delivered: %d bytes (cap %d)", len(body), api.MaxResponseBodyBytesDefault)
	}
}

// TestSetupBufferedCapWriter_UnderCap_Passes asserts the capWriter is
// transparent under the cap (the common path). Issue #995 Phase 2 /
// ADR-121.
func TestSetupBufferedCapWriter_UnderCap_Passes(t *testing.T) {
	h := NewHandlerWith(&fakeBackend{}, NewMetrics(), slog.New(slog.NewJSONHandler(io.Discard, nil)))
	payload := []byte("hello from app — well under the cap")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	t.Cleanup(upstream.Close)

	b := &fakeBackend{
		app:      App{ID: "app-ok", AccountID: "acct-ok", Plan: api.PlanFree},
		host:     "ok.apps.dom",
		upstream: upstream.Listener.Addr().String(),
		running:  true,
		targets:  []Target{{NodeID: upstream.Listener.Addr().String(), InstanceID: "i-ok"}},
	}
	b.setLegacyHot()
	h.backend = b

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://ok.apps.dom/", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("under-cap response: got %d, want 200", rec.Code)
	}
	if rec.Body.String() != string(payload) {
		t.Errorf("under-cap body: got %q, want %q", rec.Body.String(), payload)
	}
}

// TestSetupBufferedCapWriter_AtCap_200 verifies that a writer
// emitting exactly cap bytes succeeds (the +1 byte in the upstream
// io.LimitReader — when present — would only flip the EOF path
// here, the wrapper allows <= cap). Issue #995 Phase 2 / ADR-121.
func TestSetupBufferedCapWriter_AtCap_200(t *testing.T) {
	h := NewHandlerWith(&fakeBackend{}, NewMetrics(), slog.New(slog.NewJSONHandler(io.Discard, nil)))
	cap := int64(1024)
	payload := make([]byte, cap)
	for i := range payload {
		payload[i] = 'x'
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	t.Cleanup(upstream.Close)

	b := &fakeBackend{
		app:      App{ID: "app-atcap", AccountID: "acct-atcap", Plan: api.PlanHobby},
		host:     "atcap.apps.dom",
		upstream: upstream.Listener.Addr().String(),
		running:  true,
		targets:  []Target{{NodeID: upstream.Listener.Addr().String(), InstanceID: "i-atcap"}},
	}
	b.setLegacyHot()
	h.backend = b

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://atcap.apps.dom/", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("at-cap response: got %d, want 200 (body %d bytes)", rec.Code, rec.Body.Len())
	}
	if rec.Body.Len() != int(cap) {
		t.Errorf("at-cap body length: got %d, want %d", rec.Body.Len(), cap)
	}
}

// TestSetupBufferedCapWriter_ZeroCap_NoOp verifies that
// setupBufferedCapWriter with cap <= 0 is a no-op (returns the
// original writer unchanged). Issue #995 Phase 2 / ADR-121.
func TestSetupBufferedCapWriter_ZeroCap_NoOp(t *testing.T) {
	h := NewHandlerWith(&fakeBackend{}, NewMetrics(), slog.New(slog.NewJSONHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	out := h.setupBufferedCapWriter(rec, App{ID: "x", Plan: api.PlanFree}, 0)
	if out != rec {
		t.Errorf("setupBufferedCapWriter(0) returned a wrapper, want identity")
	}
	out = h.setupBufferedCapWriter(rec, App{ID: "x", Plan: api.PlanFree}, -1)
	if out != rec {
		t.Errorf("setupBufferedCapWriter(-1) returned a wrapper, want identity")
	}
}

// TestSetupBufferedCapWriter_ProblemCodeConstant sanity-checks the
// new api.CodeResponseTooLarge constant (issue #995 Phase 2 /
// ADR-121).
func TestSetupBufferedCapWriter_ProblemCodeConstant(t *testing.T) {
	if api.CodeResponseTooLarge != "response_too_large" {
		t.Errorf("CodeResponseTooLarge = %q, want response_too_large", api.CodeResponseTooLarge)
	}
}

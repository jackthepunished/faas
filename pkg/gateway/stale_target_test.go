package gateway

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
)

type staleTargetTestBackend struct {
	*fakeBackend
	evictedApp      string
	evictedInstance string
}

func (b *staleTargetTestBackend) EvictInstance(appID, instanceID string) {
	b.evictedApp = appID
	b.evictedInstance = instanceID
}

func TestHandlerEvictsOnlyForwarderMarkedStaleTarget(t *testing.T) {
	b := &staleTargetTestBackend{fakeBackend: &fakeBackend{
		app:      App{ID: "app-1", AccountID: "acct-1", Plan: api.PlanPro},
		host:     "app.example.com",
		upstream: "node-1",
		running:  true,
	}}
	h := NewHandlerWith(b, NewMetrics(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.WithForwarding(func(_ Target) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			markStaleTarget(r.Context())
			http.Error(w, "instance gone", http.StatusServiceUnavailable)
		})
	})

	req := httptest.NewRequest(http.MethodGet, "http://app.example.com/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if b.evictedApp != "app-1" || b.evictedInstance != "i-fake" {
		t.Fatalf("eviction = (%q, %q), want (app-1, i-fake)", b.evictedApp, b.evictedInstance)
	}
}

func TestHandlerDoesNotEvictOrdinaryGuestError(t *testing.T) {
	b := &staleTargetTestBackend{fakeBackend: &fakeBackend{
		app:      App{ID: "app-1", AccountID: "acct-1", Plan: api.PlanPro},
		host:     "app.example.com",
		upstream: "node-1",
		running:  true,
	}}
	h := NewHandlerWith(b, NewMetrics(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.WithForwarding(func(_ Target) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "guest error", http.StatusBadGateway)
		})
	})

	req := httptest.NewRequest(http.MethodGet, "http://app.example.com/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if b.evictedApp != "" || b.evictedInstance != "" {
		t.Fatalf("ordinary guest error evicted target: (%q, %q)", b.evictedApp, b.evictedInstance)
	}
}

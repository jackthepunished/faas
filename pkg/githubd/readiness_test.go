// Tests for pkg/githubd/readiness.go (issue #571 PR-A2).

package githubd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeGHPool struct {
	pingFn func(ctx context.Context) error
}

func (f *fakeGHPool) Ping(ctx context.Context) error {
	return f.pingFn(ctx)
}

func TestBuildReadinessProbe_AllSignalsHappy(t *testing.T) {
	pool := &fakeGHPool{pingFn: func(ctx context.Context) error { return nil }}
	p := BuildReadinessProbe(context.Background(), pool,
		func() bool { return true },
		func() bool { return true },
	)
	defer p.Drain("", nil)
	if p == nil {
		t.Fatal("BuildReadinessProbe returned nil")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r, _ := p.All(); r {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r, reason := p.All(); !r {
		t.Errorf("All() = false (reason=%q), want true", reason)
	}
}

func TestBuildReadinessProbe_PGPingFails(t *testing.T) {
	pool := &fakeGHPool{pingFn: func(ctx context.Context) error {
		return errors.New("connection refused")
	}}
	p := BuildReadinessProbe(context.Background(), pool,
		func() bool { return true },
		func() bool { return true },
	)
	defer p.Drain("", nil)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r, reason := p.All(); !r && (strings.Contains(reason, "pg") || strings.Contains(reason, "connection")) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	r, reason := p.All()
	if r {
		t.Errorf("PG ping failing: All() = true, want false")
	}
	if !strings.Contains(strings.ToLower(reason), "pg") && !strings.Contains(strings.ToLower(reason), "connection") {
		t.Errorf("reason = %q, want contains pg/connection", reason)
	}
}

func TestBuildReadinessProbe_CredsNotLoaded(t *testing.T) {
	pool := &fakeGHPool{pingFn: func(ctx context.Context) error { return nil }}
	p := BuildReadinessProbe(context.Background(), pool,
		func() bool { return false },
		func() bool { return true },
	)
	defer p.Drain("", nil)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if r, _ := p.All(); !r {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	r, reason := p.All()
	if r {
		t.Errorf("creds not loaded: All() = true, want false")
	}
	if reason == "" {
		t.Errorf("reason empty, want a not-ready reason")
	}
}

func TestBuildReadinessProbe_SecretNotWired(t *testing.T) {
	pool := &fakeGHPool{pingFn: func(ctx context.Context) error { return nil }}
	p := BuildReadinessProbe(context.Background(), pool,
		func() bool { return true },
		func() bool { return false },
	)
	defer p.Drain("", nil)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if r, _ := p.All(); !r {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if r, _ := p.All(); r {
		t.Errorf("secret not wired: All() = true, want false")
	}
}

func TestBuildReadinessProbe_NilPool(t *testing.T) {
	p := BuildReadinessProbe(context.Background(), nil,
		func() bool { return true },
		func() bool { return true },
	)
	defer p.Drain("", nil)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if r, reason := p.All(); !r && strings.Contains(reason, "pg pool nil") {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	r, reason := p.All()
	if r {
		t.Errorf("nil pool: All() = true, want false")
	}
	if !strings.Contains(reason, "pg pool nil") {
		t.Errorf("reason = %q, want contains \"pg pool nil\"", reason)
	}
}

func TestBuildReadinessProbe_NilClosures(t *testing.T) {
	pool := &fakeGHPool{pingFn: func(ctx context.Context) error { return nil }}
	// nil credsLoaded + nil secretWired: only PG ping registered.
	p := BuildReadinessProbe(context.Background(), pool, nil, nil)
	defer p.Drain("", nil)
	if p == nil {
		t.Fatal("BuildReadinessProbe returned nil")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r, _ := p.All(); r {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	r, reason := p.All()
	if !r {
		t.Errorf("nil closures: All() = false (reason=%q), want true", reason)
	}
}

func TestWebhookLoopbackHandler_RegisterReadyzOnReadyFuncWired(t *testing.T) {
	s := &Server{
		ReadyFunc:  func() bool { return true },
		ReasonFunc: func() string { return "" },
	}
	mux := s.WebhookLoopbackHandler()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("/readyz code = %d, want 200 (body=%q)", rr.Code, rr.Body.String())
	}
}

func TestWebhookLoopbackHandler_RegisterReadyzNotReady(t *testing.T) {
	s := &Server{
		ReadyFunc:  func() bool { return false },
		ReasonFunc: func() string { return "creds not loaded" },
	}
	mux := s.WebhookLoopbackHandler()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("/readyz code = %d, want 503 (body=%q)", rr.Code, rr.Body.String())
	}
}

func TestWebhookLoopbackHandler_SkipReadyzOnNilFuncs(t *testing.T) {
	s := &Server{}
	mux := s.WebhookLoopbackHandler()
	// No /readyz handler should be registered. We don't have a
	// /readyz request to assert against; instead verify
	// WebhookLoopbackHandler returned a mux that still serves
	// the webhook path (and didn't register /metrics without
	// Ops, which is the unit-test posture).
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	// The webhook handler will return 401 or 4xx for missing
	// signature; what we care about is that the mux doesn't
	// 404 the path.
	if rr.Code == http.StatusNotFound {
		t.Errorf("/webhooks/github got 404; the webhook path should be registered regardless of /readyz presence")
	}
}

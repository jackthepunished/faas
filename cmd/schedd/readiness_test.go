// Tests for cmd/schedd/readiness.go (issue #571 PR-A2).

package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/wire"
)

// fakePGPool is a stand-in for *pgxpool.Pool that satisfies
// the pgPool interface for unit tests without importing pgxpool.
type fakePGPool struct {
	mu   sync.Mutex
	err  error
	hits int
}

func (f *fakePGPool) Ping(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits++
	return f.err
}

func TestBuildReadinessProbe_NilPool(t *testing.T) {
	// Unit-test path: no real pool. The probe must NOT panic
	// on pool=nil and must surface "pg pool nil (test path)"
	// as the failing reason.
	p, bound, stop := BuildReadinessProbe(context.Background(), nil, time.Second)
	defer stop()
	if p == nil || bound == nil {
		t.Fatal("BuildReadinessProbe returned nil")
	}
	if r, reason := p.All(); r {
		t.Errorf("nil pool: probe All() = true, want false (reason should be pg pool nil)")
	} else if !strings.Contains(reason, "pg pool nil") {
		t.Errorf("reason = %q, want contains \"pg pool nil\"", reason)
	}
	bound.MarkBound()
	if r, reason := p.All(); r {
		t.Errorf("after MarkBound (nil pool): All() = true, want false (pg signal still down)")
	} else if !strings.Contains(reason, "pg pool nil") {
		t.Errorf("reason = %q, want contains \"pg pool nil\"", reason)
	}
}

func TestBuildReadinessProbe_PGHealthy(t *testing.T) {
	pool := &fakePGPool{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p, bound, stop := BuildReadinessProbe(ctx, pool, 50*time.Millisecond)
	defer stop()
	bound.MarkBound()
	// Wait for the first ping to succeed.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if r, _ := p.All(); r {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if r, reason := p.All(); !r {
		t.Errorf("healthy PG + bound gRPC: All() = false (reason=%q), want true", reason)
	}
}

func TestBuildReadinessProbe_PGFailsFlipsNotReady(t *testing.T) {
	pool := &fakePGPool{err: errors.New("connection refused")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p, bound, stop := BuildReadinessProbe(ctx, pool, 30*time.Millisecond)
	defer stop()
	bound.MarkBound()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if r, reason := p.All(); !r && strings.Contains(reason, "connection refused") {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	r, reason := p.All()
	if r {
		t.Errorf("failing PG: All() = true, want false")
	}
	if !strings.Contains(reason, "connection refused") {
		t.Errorf("reason = %q, want contains \"connection refused\"", reason)
	}
}

func TestGrpcBoundSignal_StartsNotReady(t *testing.T) {
	_, bound, stop := BuildReadinessProbe(context.Background(), &fakePGPool{}, time.Second)
	defer stop()
	r, reason := bound.Signal().Report()
	if r {
		t.Errorf("before MarkBound: ready = true (reason=%q), want false", reason)
	}
	if !strings.Contains(reason, "grpc not yet bound") {
		t.Errorf("initial reason = %q, want contains \"grpc not yet bound\"", reason)
	}
}

func TestGrpcBoundSignal_MarkBound(t *testing.T) {
	_, bound, stop := BuildReadinessProbe(context.Background(), &fakePGPool{}, time.Second)
	defer stop()
	bound.MarkBound()
	if r, _ := bound.Signal().Report(); !r {
		t.Errorf("after MarkBound: ready = false, want true")
	}
}

func TestBuildReadinessProbe_EndToEndViaControlMuxLite(t *testing.T) {
	pool := &fakePGPool{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p, bound, stop := BuildReadinessProbe(ctx, pool, 50*time.Millisecond)
	defer stop()
	bound.MarkBound()
	// Wait for first ping to succeed.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if r, _ := p.All(); r {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mux := http.NewServeMux()
	wire.ControlMuxLite(mux, p.ReadyFunc(), p.ReasonFunc())
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("/readyz code = %d, want 200 (body=%q)", rr.Code, rr.Body.String())
	}
	if body := rr.Body.String(); body != "ready" {
		t.Errorf("/readyz body = %q, want \"ready\"", body)
	}
}

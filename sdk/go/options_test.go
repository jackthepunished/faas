// Package faas SDK options tests (issue #560 / ADR-080 follow-up).
//
// These tests pin WithToken's wiring end-to-end against a stub
// httptest server: the option calls SetToken on the embedded api
// client; subsequent requests send the new bearer. The race-detector
// variant exercises concurrent SetToken + in-flight request builders
// to confirm the RWMutex protects the token field.

package faas

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestWithToken_OverridesInitialToken confirms WithToken applies a new
// bearer at construction time. Initial token = "old"; WithToken("new")
// must produce a request that sends Authorization: Bearer new.
func TestWithToken_OverridesInitialToken(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "old", WithToken("new"))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if got := c.Token(); got != "new" {
		t.Errorf("Token() = %q, want \"new\"", got)
	}
	if _, err := c.GetApp(context.Background(), "demo"); err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if seen != "Bearer new" {
		t.Errorf("request Authorization = %q, want \"Bearer new\"", seen)
	}
}

// TestWithToken_EmptySuppressesHeader confirms WithToken("") produces a
// Client that does NOT send Authorization at all (the device-code
// flow's anonymous mode).
func TestWithToken_EmptySuppressesHeader(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "old", WithToken(""))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.Token() != "" {
		t.Errorf("Token() = %q, want \"\"", c.Token())
	}
	if _, err := c.GetApp(context.Background(), "demo"); err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if got != "" {
		t.Errorf("Authorization header = %q, want \"\" (WithToken(\"\") should suppress)", got)
	}
}

// TestSetToken_AfterConstruction rotates the token mid-session via
// SetToken on the embedded api.Client. Mirrors the Node SDK's
// setToken() — Python SDK has no helper yet (issue #560 deferred).
func TestSetToken_AfterConstruction(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "first")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := c.GetApp(context.Background(), "demo"); err != nil {
		t.Fatalf("first GetApp: %v", err)
	}
	c.Client.SetToken("second")
	if got := c.Token(); got != "second" {
		t.Errorf("Token() after SetToken = %q, want \"second\"", got)
	}
	if _, err := c.GetApp(context.Background(), "demo"); err != nil {
		t.Fatalf("second GetApp: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("server saw %d requests, want 2", len(seen))
	}
	if seen[0] != "Bearer first" {
		t.Errorf("seen[0] = %q, want \"Bearer first\"", seen[0])
	}
	if seen[1] != "Bearer second" {
		t.Errorf("seen[1] = %q, want \"Bearer second\"", seen[1])
	}
}

// TestSetToken_ConcurrentSafe runs SetToken in one goroutine and
// concurrent GetApp calls in many others; the test fails on -race if
// the lock is insufficient. This is the regression gate for issue
// #560 — the prior stub (errOptionUnsupported) made rotation
// impossible; now it works under contention.
func TestSetToken_ConcurrentSafe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Echo the auth header so a reader (the test goroutine) can
		// at least observe a non-empty value when the token is set.
		// We don't assert which token — concurrency means any of the
		// rotated values are valid; the point is no data race.
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "t0")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	const writers, readers = 4, 16
	var wg sync.WaitGroup
	wg.Add(writers + readers)
	for i := 0; i < writers; i++ {
		i := i
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				c.Client.SetToken("writer-" + string(rune('A'+i)) + "-" + strings.Repeat("x", j%4))
			}
		}()
	}
	for i := 0; i < readers; i++ {
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*1e9)
			defer cancel()
			for j := 0; j < 50; j++ {
				_, _ = c.GetApp(ctx, "demo")
			}
		}()
	}
	wg.Wait()
}

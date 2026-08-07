// Tests for the SIGHUP-driven egress bundle reload (issue #679 /
// PR-A). The watcher is defined in egress_bundle.go and dispatches
// to a narrow interface (egressBundleTarget) so we can stub the
// Manager without booting a real fcvm.Manager.

package main

import (
	"context"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

// stubEgressBundleTarget is the SIGHUP-reload test sink. It
// implements egressBundleTarget with a copy-under-mutex
// observed slice so the test can race-free assert what the
// watcher handed it.
type stubEgressBundleTarget struct {
	mu    sync.Mutex
	cidrs []netip.Prefix
	calls int
}

func (s *stubEgressBundleTarget) SetEgressOperatorBundle(cidrs []netip.Prefix) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Defensive copy; the watcher hands us the slice it
	// intends to install, so we own it now.
	cp := make([]netip.Prefix, len(cidrs))
	copy(cp, cidrs)
	s.cidrs = cp
	s.calls++
}

func (s *stubEgressBundleTarget) snapshot() ([]netip.Prefix, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]netip.Prefix, len(s.cidrs))
	copy(cp, s.cidrs)
	return cp, s.calls
}

func silentSighupLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestWatchEgressBundleReload_HupAppliesNewBundle verifies the
// happy path: write a bundle file, send SIGHUP, observe the
// stub's CIDRs reflect the file contents. The watcher also
// performs an initial load at startup (ADR-081 / PR-A contract:
// "startup AND on SIGHUP"), so the call count is 2 here:
// startup-load + SIGHUP-load.
func TestWatchEgressBundleReload_HupAppliesNewBundle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "operator_allowlist.toml")
	if err := os.WriteFile(path, []byte(`cidrs = ["203.0.113.0/24", "198.51.100.0/24"]`+"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	stub := &stubEgressBundleTarget{}
	hup := make(chan os.Signal, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go watchEgressBundleReload(ctx, stub, path, silentSighupLogger(), hup)

	// Wait for the goroutine's startup load to land.
	waitForCalls(t, stub, 1, 2*time.Second)

	// SIGHUP re-applies the same bundle (refines). Second call.
	hup <- syscall.SIGHUP
	waitForCalls(t, stub, 2, 2*time.Second)

	got, calls := stub.snapshot()
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (startup + SIGHUP)", calls)
	}
	if len(got) != 2 {
		t.Fatalf("cidrs len = %d, want 2; got=%v", len(got), got)
	}
	want1 := []netip.Prefix{
		netip.MustParsePrefix("198.51.100.0/24"),
		netip.MustParsePrefix("203.0.113.0/24"),
	}
	for i, p := range got {
		if p != want1[i] {
			t.Errorf("cidrs[%d] = %s, want %s", i, p, want1[i])
		}
	}
}

// TestWatchEgressBundleReload_StartupLoadWithoutSighup pins
// the "startup AND on SIGHUP" contract: a fresh watcher applies
// the bundle from the file at startup WITHOUT needing a SIGHUP.
// This is the bug fix for PR #723 review finding #1 — pre-fix,
// the watcher only acted on SIGHUP, so a fresh vmmd with a
// configured bundle sat empty until an operator manually
// HUP'd.
func TestWatchEgressBundleReload_StartupLoadWithoutSighup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "operator_allowlist.toml")
	if err := os.WriteFile(path, []byte(`cidrs = ["203.0.113.0/24"]`+"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	stub := &stubEgressBundleTarget{}
	hup := make(chan os.Signal, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go watchEgressBundleReload(ctx, stub, path, silentSighupLogger(), hup)

	waitForCalls(t, stub, 1, 2*time.Second)

	got, calls := stub.snapshot()
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (startup load, no SIGHUP required)", calls)
	}
	if len(got) != 1 {
		t.Fatalf("cidrs len = %d, want 1; got=%v", len(got), got)
	}
	if got[0] != netip.MustParsePrefix("203.0.113.0/24") {
		t.Errorf("cidrs[0] = %s, want 203.0.113.0/24", got[0])
	}
}

// TestWatchEgressBundleReload_MultipleHups verifies a second
// signal picks up the file's new contents. 3 calls total: startup
// + 2 SIGHUPs.
func TestWatchEgressBundleReload_MultipleHups(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "operator_allowlist.toml")
	// 0o644 (not 0o400) so the test can overwrite the file
	// mid-test. Production would use 0o400 root-only; the
	// loader doesn't care.
	if err := os.WriteFile(path, []byte(`cidrs = ["203.0.113.0/24"]`+"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	stub := &stubEgressBundleTarget{}
	hup := make(chan os.Signal, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go watchEgressBundleReload(ctx, stub, path, silentSighupLogger(), hup)

	// Startup load.
	waitForCalls(t, stub, 1, 2*time.Second)

	// Overwrite the file with a different bundle, then SIGHUP.
	if err := os.WriteFile(path, []byte(`cidrs = ["10.0.0.0/8", "192.0.2.0/24"]`+"\n"), 0o400); err != nil {
		t.Fatalf("write: %v", err)
	}
	hup <- syscall.SIGHUP
	waitForCalls(t, stub, 2, 2*time.Second)
	// Second SIGHUP — file is unchanged, but the watcher still
	// re-loads. Call count = 3.
	hup <- syscall.SIGHUP
	waitForCalls(t, stub, 3, 2*time.Second)

	got, calls := stub.snapshot()
	if calls != 3 {
		t.Fatalf("calls = %d, want 3 (startup + 2 SIGHUPs)", calls)
	}
	if len(got) != 2 {
		t.Fatalf("cidrs len = %d, want 2; got=%v", len(got), got)
	}
	want := []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("192.0.2.0/24"),
	}
	for i, p := range got {
		if p != want[i] {
			t.Errorf("cidrs[%d] = %s, want %s", i, p, want[i])
		}
	}
}

// TestWatchEgressBundleReload_EmptyPathDisablesGoroutine pins
// the "no operator bundle configured" path: an empty path means
// the watcher exits without ever calling SetEgressOperatorBundle,
// even after a SIGHUP.
func TestWatchEgressBundleReload_EmptyPathDisablesGoroutine(t *testing.T) {
	stub := &stubEgressBundleTarget{}
	hup := make(chan os.Signal, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// No goroutine started — the function returns immediately
	// for empty path. Verify by timing the return.
	go watchEgressBundleReload(ctx, stub, "", silentSighupLogger(), hup)

	// The function returns instantly; verify the stub stays
	// untouched even after sending a SIGHUP.
	hup <- syscall.SIGHUP
	time.Sleep(100 * time.Millisecond)

	_, calls := stub.snapshot()
	if calls != 0 {
		t.Errorf("calls = %d, want 0 (empty path = no reload)", calls)
	}
}

// TestWatchEgressBundleReload_MalformedTomlKeepsPriorBundle
// pins the "best-effort reload" contract: a failed reload
// leaves the prior bundle live (the watcher does NOT call
// SetEgressOperatorBundle with an empty slice on parse error).
// The startup load here succeeds, then a SIGHUP-triggered
// reload fails on a corrupted file — the prior bundle stays.
func TestWatchEgressBundleReload_MalformedTomlKeepsPriorBundle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "operator_allowlist.toml")
	// First: a valid bundle so the watcher has a "prior" state.
	// 0o644 (not 0o400) so the test can overwrite the file mid-test.
	if err := os.WriteFile(path, []byte(`cidrs = ["203.0.113.0/24"]`+"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	stub := &stubEgressBundleTarget{}
	hup := make(chan os.Signal, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go watchEgressBundleReload(ctx, stub, path, silentSighupLogger(), hup)

	// Startup load succeeds.
	waitForCalls(t, stub, 1, 2*time.Second)

	// Now corrupt the file.
	if err := os.WriteFile(path, []byte("this is = not [valid toml"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	hup <- syscall.SIGHUP
	// Give it a beat to attempt the (failed) reload.
	time.Sleep(200 * time.Millisecond)

	got, calls := stub.snapshot()
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (malformed TOML = no set call beyond startup)", calls)
	}
	if len(got) != 1 {
		t.Fatalf("cidrs len = %d, want 1 (prior bundle preserved); got=%v", len(got), got)
	}
	if got[0] != netip.MustParsePrefix("203.0.113.0/24") {
		t.Errorf("cidrs[0] = %s, want 203.0.113.0/24", got[0])
	}
}

// TestWatchEgressBundleReload_StartupMalformedTomlKeepsEmpty
// pins the startup-load path's "best-effort" sibling: a
// malformed vmmd.toml EgressOperatorAllowlist on a fresh boot
// must NOT crash vmmd nor block startup. The watcher logs a
// Warn and the manager bundle stays empty (Call count = 0).
func TestWatchEgressBundleReload_StartupMalformedTomlKeepsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "operator_allowlist.toml")
	if err := os.WriteFile(path, []byte("this is = not [valid toml"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	stub := &stubEgressBundleTarget{}
	hup := make(chan os.Signal, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go watchEgressBundleReload(ctx, stub, path, silentSighupLogger(), hup)

	// Give the startup-load a beat to attempt + fail.
	time.Sleep(200 * time.Millisecond)

	_, calls := stub.snapshot()
	if calls != 0 {
		t.Errorf("calls = %d, want 0 (malformed startup = no set call)", calls)
	}
}

// TestWatchEgressBundleReload_ContextCancelExits verifies the
// goroutine is responsive to ctx.Done() (no SIGHUP needed).
func TestWatchEgressBundleReload_ContextCancelExits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "operator_allowlist.toml")
	if err := os.WriteFile(path, []byte(`cidrs = ["203.0.113.0/24"]`+"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	stub := &stubEgressBundleTarget{}
	hup := make(chan os.Signal, 1)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		watchEgressBundleReload(ctx, stub, path, silentSighupLogger(), hup)
		close(done)
	}()
	cancel()
	select {
	case <-done:
		// Goroutine exited cleanly.
	case <-time.After(2 * time.Second):
		t.Fatal("watchEgressBundleReload did not exit on ctx.Done()")
	}
}

func waitForCalls(t *testing.T, stub *stubEgressBundleTarget, want int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, calls := stub.snapshot(); calls >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, calls := stub.snapshot()
	t.Fatalf("calls = %d, want >= %d after %s", calls, want, within)
}

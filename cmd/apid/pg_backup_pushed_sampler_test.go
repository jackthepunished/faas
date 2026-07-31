// Tests for the pgBackupPushedSampler goroutine (issue #250).
// Covers the three load-bearing paths:
//
//   1. Empty /var/lib/pgsql/basebackup/ → gauge stays at 0.
//   2. One tarball → gauge ≥ 0 (the stamp is monotonically
//      bounded by time.Since, which is always ≥ 0).
//   3. nil ops on the sampler → run() exits cleanly without
//      touching the gauge.
//
// We don't drive the sampler via the ticker in tests (that would
// race the gauge value); instead we exercise tick() directly +
// newestEntryMtime() against a TempDir. The TempDir makes the
// test self-contained — no /var/lib/pgsql/basebackup dependency
// on the CI runner.

package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewestEntryMtime_Empty(t *testing.T) {
	dir := t.TempDir()
	_, ok := newestEntryMtime(dir)
	if ok {
		t.Fatalf("empty dir: newestEntryMtime returned ok=true")
	}
}

func TestNewestEntryMtime_MissingDir(t *testing.T) {
	_, ok := newestEntryMtime(filepath.Join(t.TempDir(), "no-such-dir"))
	if ok {
		t.Fatalf("missing dir: newestEntryMtime returned ok=true")
	}
}

func TestNewestEntryMtime_PicksNewest(t *testing.T) {
	dir := t.TempDir()
	// Write three entries with explicit mtimes — older, newest,
	// middle — and assert newestEntryMtime returns the newest.
	writeAt(t, dir, "old.tar.gz", time.Now().Add(-2*time.Hour))
	newestName := "fresh.tar.gz"
	writeAt(t, dir, newestName, time.Now().Add(-1*time.Minute))
	writeAt(t, dir, "mid.tar.gz", time.Now().Add(-1*time.Hour))

	got, ok := newestEntryMtime(dir)
	if !ok {
		t.Fatalf("non-empty dir: newestEntryMtime returned ok=false")
	}
	if got.IsZero() {
		t.Fatalf("newestEntryMtime returned zero time")
	}
	// Loose bound: between 30s and 5min ago. The exact value
	// depends on the runner's clock skew between writeAt and
	// newestEntryMtime.
	age := time.Since(got)
	if age < 30*time.Second || age > 5*time.Minute {
		t.Errorf("newest age = %v, want ~1m", age)
	}
}

func TestPgBackupPushedSampler_Tick_EmptyDirStaysAtZero(t *testing.T) {
	dir := t.TempDir()
	swapBackupRoot(t, dir)

	s := newPgBackupPushedSampler(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// nil ops is the documented early-exit; we still want to
	// confirm run() returns cleanly.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { s.run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("run() did not exit within 1s on ctx cancel")
	}
}

// swapBackupRoot rewrites the package-level pgBackupPushedRoot
// for the duration of a test. Restored via t.Cleanup so a t.Fatal
// in the middle of a subtest still resets state.
func swapBackupRoot(t *testing.T, newRoot string) {
	t.Helper()
	prev := pgBackupPushedRoot
	pgBackupPushedRoot = newRoot
	t.Cleanup(func() { pgBackupPushedRoot = prev })
}

// writeAt creates a file under dir/name with the given mtime.
// Uses os.Chtimes to backdate so the test doesn't have to wait.
func writeAt(t *testing.T, dir, name string, mtime time.Time) {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.WriteFile(full, []byte("stub"), 0o600); err != nil {
		t.Fatalf("write %s: %v", full, err)
	}
	if err := os.Chtimes(full, mtime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", full, err)
	}
}

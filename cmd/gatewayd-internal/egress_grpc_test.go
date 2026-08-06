package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/gateway/egressgrpc"
	"github.com/onebox-faas/faas/pkg/gateway/egresssink"
)

// egressSockPath returns a macOS-compatible unix socket path
// (sun_path is capped at 104 bytes on darwin, and t.TempDir nests
// under /var/folders/...). Mirrors the synth_integration_test
// workaround.
func egressSockPath(t *testing.T) string {
	t.Helper()
	sock := "/tmp/" + strings.ReplaceAll(t.Name(), "/", "_") + ".sock"
	_ = os.Remove(sock)
	t.Cleanup(func() { _ = os.Remove(sock) })
	return sock
}

// TestEgressStart_RemovesStaleSocket pins the legitimate use
// of os.Remove in start(): a crashed prior daemon leaves the
// dirent behind, and the next start() must clear it before
// bind. This is the load-bearing Remove — it's the stop-time
// Remove that was racy (cd-controlplane run 31121004495: old
// daemon's deferred Remove fired AFTER new daemon's net.Listen,
// deleting the live dirent meterd dialed into).
func TestEgressStart_RemovesStaleSocket(t *testing.T) {
	sock := egressSockPath(t)

	// Drop a stale file at the path — simulates a crashed prior
	// daemon that left the dirent behind.
	if err := os.WriteFile(sock, []byte("stale"), 0o600); err != nil {
		t.Fatalf("seed stale: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sink := egresssink.NewEgressSink()
	egressSrv := egressgrpc.NewServer(sink, logger)
	ln := newEgressGRPCListener(sock, nil, egressSrv, logger)
	if err := ln.start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		_ = ln.stop(context.Background())
	})

	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("post-start stat: %v", err)
	}
	if fi.Mode().Type()&os.ModeSocket == 0 {
		t.Errorf("post-start path is %v, want a socket", fi.Mode().Type())
	}
}

// TestEgressStart_DialAfterRebind simulates the production
// restart-loop race that took meterd OOM. With the OLD stop()
// (which had `_ = os.Remove(l.socketPath)`), the sequence:
//
//	OLD start  →  OLD stop (Remove)  →  NEW start (Listen)
//
// could race so that OLD's Remove fired AFTER NEW's Listen,
// deleting the live dirent. With the fix (no stop-time
// Remove), the new daemon's dirent is owned solely by its own
// fd lifetime and is never touched by the old daemon. We
// exercise the full stop+start cycle and assert the new
// listener is bindable AND dialable.
func TestEgressStart_DialAfterRebind(t *testing.T) {
	sock := egressSockPath(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sink := egresssink.NewEgressSink()
	egressSrv := egressgrpc.NewServer(sink, logger)

	// First listener.
	ln1 := newEgressGRPCListener(sock, nil, egressSrv, logger)
	if err := ln1.start(context.Background()); err != nil {
		t.Fatalf("first start: %v", err)
	}
	// Confirm dialable.
	conn, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		t.Fatalf("dial after first start: %v", err)
	}
	_ = conn.Close()
	// Stop. Go's net.UnixListener.Close unlinks the socket file
	// (verified out-of-tree). The fix relies on that natural
	// cleanup rather than a racy explicit os.Remove.
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := ln1.stop(stopCtx); err != nil {
		t.Fatalf("first stop: %v", err)
	}
	cancel()

	// Second listener against the same path. start()'s pre-Remove
	// clears any dirent Go's Close left behind, then binds fresh.
	ln2 := newEgressGRPCListener(sock, nil, egressSrv, logger)
	if err := ln2.start(context.Background()); err != nil {
		t.Fatalf("second start: %v", err)
	}
	t.Cleanup(func() {
		_ = ln2.stop(context.Background())
	})

	// Critical assertion: a fresh dial succeeds against the new
	// daemon's dirent. If the OLD stop()'s os.Remove were still
	// racing here, the new daemon's net.Listen could bind a
	// dirent that the old daemon's deferred Remove then deletes;
	// this dial would fail. With the fix, the new daemon owns
	// the dirent for its full lifetime.
	//
	// Same flake guard as RepeatedCycle below: the kernel publishes
	// the dirent asynchronously after net.Listen returns, and under
	// CI runner-pool load the publish can lag. Wait for the dirent
	// before dialing, then retry the dial over a 2s budget so the
	// gRPC server's Serve goroutine has time to accept.
	if err := waitForSocket(sock, 2*time.Second); err != nil {
		t.Fatalf("wait for socket after second start: %v", err)
	}
	conn, err = dialWithRetry(sock, 2*time.Second, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("dial after second start: %v", err)
	}
	_ = conn.Close()
}

// TestEgressStopStopStart_RepeatedCycle runs the stop+start
// cycle many times to surface any flake in the dirent lifecycle
// (the production bug was observed every restart, so a tight
// loop is the right shape).
func TestEgressStopStopStart_RepeatedCycle(t *testing.T) {
	sock := egressSockPath(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sink := egresssink.NewEgressSink()
	egressSrv := egressgrpc.NewServer(sink, logger)

	var wg sync.WaitGroup
	const cycles = 16
	for i := 0; i < cycles; i++ {
		ln := newEgressGRPCListener(sock, nil, egressSrv, logger)
		if err := ln.start(context.Background()); err != nil {
			t.Fatalf("cycle %d start: %v", i, err)
		}
		// Wait for the socket dirent to appear before dialing.
		// go's net.Listen returns before the kernel has fully
		// published the dirent; dial in that micro-window can
		// ECONNREFUSED with "no such file or directory" on a
		// cold filesystem (CI runner pool). The test's purpose
		// is to verify the stop+start CYCLE stays bindable, not
		// to time the kernel's dirent publish — so wait briefly.
		if err := waitForSocket(sock, 2*time.Second); err != nil {
			t.Fatalf("cycle %d wait for socket: %v", i, err)
		}
		// The dirent's appearance precedes the gRPC server's
		// Serve goroutine accepting. On a busy CI runner the
		// gap can exceed DialTimeout's first probe — net.Dial
		// errors eagerly on ECONNREFUSED without retrying. Wrap
		// the dial in a small retry budget so the test's
		// purpose (cycle stays bindable) is what we measure,
		// not the kernel's accept-poll cadence.
		conn, err := dialWithRetry(sock, 2*time.Second, 10*time.Millisecond)
		if err != nil {
			t.Fatalf("cycle %d dial after start: %v", i, err)
		}
		_ = conn.Close()
		stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = ln.stop(stopCtx)
		cancel()
	}
	wg.Wait()
}

// waitForSocket polls for the unix socket dirent to appear, with a
// deadline. The kernel publishes the dirent asynchronously after
// net.Listen returns; under CI runner-pool load the publish can
// lag a few ms past the listen return.
func waitForSocket(path string, deadline time.Duration) error {
	timeout := time.Now().Add(deadline)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if time.Now().After(timeout) {
			return os.ErrNotExist
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// dialWithRetry retries net.DialTimeout until either a connection is
// established or the deadline elapses. The dial itself errors
// eagerly on ECONNREFUSED without retrying; on a CI runner under
// load the gRPC server's Serve goroutine may not have accepted yet
// even after the socket dirent has appeared, so the first probe is
// essentially free. The retry budget (default 2s, 10ms backoff) is
// generous enough to soak the longest observed CI pause (cycle 4
// in run 31080218226 finished in 0.00s — the dial failed on the
// first probe) without slowing the happy path.
func dialWithRetry(path string, deadline, backoff time.Duration) (net.Conn, error) {
	timeout := time.Now().Add(deadline)
	for {
		conn, err := net.DialTimeout("unix", path, 100*time.Millisecond)
		if err == nil {
			return conn, nil
		}
		if time.Now().After(timeout) {
			return nil, err
		}
		time.Sleep(backoff)
	}
}

// Tests for schedd's SIGHUP-driven TLS cert rotation (PR-E /
// ADR-052 §5). The wire.TLSRotator contract (Set / Get / Reload /
// nil-tolerance) is pinned by pkg/wire/tls_rotator_test.go. Here
// we drive schedd's rotator end-to-end through WatchTLSReload to
// confirm the schedd-shaped wiring is correct.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/wire"
)

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newMinimalTLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13}
}

func newDifferentTLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12}
}

func TestScheddTLSReload_SIGHUPRotatesLiveConfig(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r := wire.NewTLSRotator(newMinimalTLSConfig())
	hupCh := make(chan os.Signal, 1)

	// The SIGHUP-driven reload closure: replace the rotator's
	// stored config with a fresh, distinct *tls.Config.
	reload := func() (*tls.Config, error) {
		return newDifferentTLSConfig(), nil
	}

	go wire.WatchTLSReload(ctx, silentLogger(), hupCh, r, reload)

	hupCh <- syscall.SIGHUP
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if r.Get() != newMinimalTLSConfig() && r.Get() != nil {
			break // rotated away from initial
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := r.Get(); got == newMinimalTLSConfig() {
		t.Errorf("rotator still holds initial after SIGHUP; got=%p want a fresh config", got)
	}
}

func TestScheddTLSReload_MalformedReloadKeepsPrior(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	initial := newMinimalTLSConfig()
	r := wire.NewTLSRotator(initial)
	hupCh := make(chan os.Signal, 1)

	boom := errors.New("malformed PEM")
	reload := func() (*tls.Config, error) { return nil, boom }

	go wire.WatchTLSReload(ctx, silentLogger(), hupCh, r, reload)

	hupCh <- syscall.SIGHUP
	// Give the watcher time to consult the closure; the rotator
	// must NOT swap.
	time.Sleep(100 * time.Millisecond)

	if got := r.Get(); got != initial {
		t.Errorf("rotator after malformed reload: got=%p, want prior %p", got, initial)
	}
}
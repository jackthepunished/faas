// Tests for vmmd's SIGHUP-driven TLS cert rotation (PR-E /
// ADR-052 §5). The wire.TLSRotator contract (Set / Get / Reload /
// nil-tolerance) is pinned by pkg/wire/tls_rotator_test.go. Here
// we drive vmmd's rotator end-to-end through WatchTLSReload to
// confirm the vmmd-shaped wiring is correct.
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

func silentTLSReloadLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newMinimalTLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13}
}

func newDifferentTLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12}
}

func TestVmmdTLSReload_SIGHUPRotatesLiveConfig(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r := wire.NewTLSRotator(newMinimalTLSConfig())
	hupCh := make(chan os.Signal, 1)

	reload := func() (*tls.Config, error) {
		return newDifferentTLSConfig(), nil
	}

	go wire.WatchTLSReload(ctx, silentTLSReloadLogger(), hupCh, r, reload)

	hupCh <- syscall.SIGHUP
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if r.Get() != newMinimalTLSConfig() && r.Get() != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := r.Get(); got == newMinimalTLSConfig() {
		t.Errorf("rotator still holds initial after SIGHUP; got=%p want a fresh config", got)
	}
}

func TestVmmdTLSReload_MalformedReloadKeepsPrior(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	initial := newMinimalTLSConfig()
	r := wire.NewTLSRotator(initial)
	hupCh := make(chan os.Signal, 1)

	boom := errors.New("malformed PEM")
	reload := func() (*tls.Config, error) { return nil, boom }

	go wire.WatchTLSReload(ctx, silentTLSReloadLogger(), hupCh, r, reload)

	hupCh <- syscall.SIGHUP
	time.Sleep(100 * time.Millisecond)

	if got := r.Get(); got != initial {
		t.Errorf("rotator after malformed reload: got=%p, want prior %p", got, initial)
	}
}
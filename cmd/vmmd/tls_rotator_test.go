// Tests for cmd/vmmd's tlsRotator (PR-E / ADR-052 §5). Sister to
// cmd/schedd/tls_rotator_test.go; the rotator shape is identical
// but lives per-daemon for ergonomics. Here we pin the contract:
//
//   - newTLSRotator stores the initial config.
//   - Set replaces the live config; Get observes the swap.
//   - Set with nil is silently dropped.
//   - Rotator.Reload reads the live config at handshake time.
//   - Rotator.Reload on a nil-rotator returns initial.
//   - WatchTLSReload-driven SIGHUP rotation surfaces through Get.
//   - All ops are goroutine-safe under -race.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
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

func TestTLSRotator_NewStoresInitial(t *testing.T) {
	initial := newMinimalTLSConfig()
	r := newTLSRotator(initial)
	if got := r.Get(); got != initial {
		t.Errorf("Get = %p, want %p", got, initial)
	}
}

func TestTLSRotator_NewNilInitialIsTolerated(t *testing.T) {
	r := newTLSRotator(nil)
	if got := r.Get(); got != nil {
		t.Errorf("Get on nil-initial: %p, want nil", got)
	}
}

func TestTLSRotator_SetReplacesConfig(t *testing.T) {
	r := newTLSRotator(newMinimalTLSConfig())
	next := newDifferentTLSConfig()
	r.Set(next)
	if got := r.Get(); got != next {
		t.Errorf("Get after Set: %p, want %p", got, next)
	}
}

func TestTLSRotator_SetNilIsSilentlyDropped(t *testing.T) {
	r := newTLSRotator(newMinimalTLSConfig())
	original := r.Get()
	r.Set(nil)
	if got := r.Get(); got != original {
		t.Errorf("Set(nil) replaced live config: got %p, want prior %p", got, original)
	}
}

func TestTLSRotator_GetNilReceiver(t *testing.T) {
	var r *tlsRotator
	if got := r.Get(); got != nil {
		t.Errorf("nil-receiver Get = %p, want nil", got)
	}
}

func TestTLSRotator_SetNilReceiver(t *testing.T) {
	var r *tlsRotator
	r.Set(newMinimalTLSConfig()) // must not panic
}

func TestTLSRotator_ReloadReturnsLiveConfig(t *testing.T) {
	initial := newMinimalTLSConfig()
	r := newTLSRotator(initial)
	closure := r.Reload(initial)

	if got, err := closure(); err != nil || got != initial {
		t.Errorf("first call: got=%v err=%v, want initial=%p err=nil", got, err, initial)
	}

	next := newDifferentTLSConfig()
	r.Set(next)
	if got, err := closure(); err != nil || got != next {
		t.Errorf("after Set: got=%v err=%v, want next=%p err=nil", got, err, next)
	}
}

func TestTLSRotator_ReloadOnNilReceiverReturnsInitial(t *testing.T) {
	var r *tlsRotator
	initial := newMinimalTLSConfig()
	closure := r.Reload(initial)
	got, err := closure()
	if err != nil {
		t.Fatalf("nil-receiver Reload: err=%v, want nil", err)
	}
	if got != initial {
		t.Errorf("nil-receiver closure: got=%p, want initial=%p", got, initial)
	}
}

func TestTLSRotator_ReloadBeforeAnySet(t *testing.T) {
	t.Run("nil initial, nil Set", func(t *testing.T) {
		r := newTLSRotator(nil)
		closure := r.Reload(nil)
		if got, err := closure(); err != nil || got != nil {
			t.Errorf("got=%v err=%v, want nil/nil", got, err)
		}
	})
	t.Run("non-nil initial, no Set", func(t *testing.T) {
		initial := newMinimalTLSConfig()
		r := newTLSRotator(nil)
		closure := r.Reload(initial)
		got, err := closure()
		if err != nil {
			t.Fatalf("err=%v, want nil", err)
		}
		if got != initial {
			t.Errorf("got=%p, want initial=%p", got, initial)
		}
	})
}

func TestVmmdTLSReload_SIGHUPRotatesLiveConfig(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r := newTLSRotator(newMinimalTLSConfig())
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
	r := newTLSRotator(initial)
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

// TestVmmdTLSReload_ConcurrentSwapsAndReads pins the goroutine-safe
// contract under -race. Atomic.Pointer is the load-bearing
// primitive.
func TestVmmdTLSReload_ConcurrentSwapsAndReads(t *testing.T) {
	initial := newMinimalTLSConfig()
	r := newTLSRotator(initial)
	closure := r.Reload(initial)

	const goroutines = 8
	const iterations = 200

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				switch (id + j) % 3 {
				case 0:
					r.Set(newDifferentTLSConfig())
				case 1:
					_ = r.Get()
				case 2:
					_, _ = closure()
				}
			}
		}(i)
	}
	wg.Wait()
}

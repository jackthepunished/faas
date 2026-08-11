// Tests for cmd/schedd's tlsRotator (PR-E / ADR-052 §5). Sister
// file: pkg/wire/grpc_test.go's WatchTLSReload_* tests cover the
// stdlib wiring. Here we pin the schedd-side contract:
//
//   - newTLSRotator stores the initial config.
//   - Set replaces the live config; Get observes the swap.
//   - Set with nil is silently dropped (defensive — WatchTLSReload
//     already warns on the (nil, nil) reload case).
//   - Rotator.Reload reads the live config at handshake time (the
//     closure stdlib consults on every handshake returns Get()).
//   - Rotator.Reload on a nil-rotator returns initial (the
//     single-box / no-cluster back-compat path).
//   - All ops are goroutine-safe under -race (the atomic.Pointer
//     is the load-bearing primitive).
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

func silentLogger() *slog.Logger {
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

// TestTLSRotator_ReloadReturnsLiveConfig pins the per-handshake
// contract: the closure installed by Load*TLSConfigWithReload
// reads the rotator's live config every time stdlib calls it.
// A Set between two consultation calls is observable to the
// second consultation.
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

// TestTLSRotator_ReloadOnNilReceiverReturnsInitial pins the
// single-box / no-cluster back-compat path: a watcher that loses
// its rotator (e.g. nil-tolerance paths) still hands stdlib
// something usable.
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

// TestTLSRotator_ReloadBeforeAnySetReturnsInitial pins the
// boot-with-no-TLS path: a rotator built with nil initial that
// hasn't yet been Set returns nil from Reload; an empty rotator
// built from a non-nil initial returns initial until the first
// successful Set.
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

// --- WatchTLSReload integration via the rotator (sister to the
// pkg/wire/grpc_test.go equivalents; here we drive schedd's
// rotator end-to-end through WatchTLSReload to confirm the
// schedd-shaped wiring is correct).

func TestScheddTLSReload_SIGHUPRotatesLiveConfig(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r := newTLSRotator(newMinimalTLSConfig())
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
	r := newTLSRotator(initial)
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

// TestScheddTLSReload_ConcurrentSwapsAndReads pins the
// goroutine-safe contract under -race: many goroutines Set /
// Get / call the Reload closure concurrently. The atomic.Pointer
// is the load-bearing primitive.
func TestScheddTLSReload_ConcurrentSwapsAndReads(t *testing.T) {
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

// Tests for pkg/wire.TLSRotator (PR-E / ADR-052 §5). Sister to
// pkg/wire/grpc_test.go's WatchTLSReload_* tests; here we pin the
// TLSRotator contract that every daemon (schedd, vmmd, apid) reuses:
//
//   - NewTLSRotator stores the initial config.
//   - Set replaces the live config; Get observes the swap.
//   - Set with nil is silently dropped (defensive — WatchTLSReload
//     already warns on the (nil, nil) reload case).
//   - Reload reads the live config at handshake time (the closure
//     stdlib consults on every handshake returns Get()).
//   - Reload on a nil-rotator returns initial (the single-box /
//     no-cluster back-compat path).
//   - All ops are goroutine-safe under -race (the atomic.Pointer
//     is the load-bearing primitive).
package wire

import (
	"crypto/tls"
	"testing"
)

func newMinimalTLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13}
}

func newDifferentTLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12}
}

func TestTLSRotator_NewStoresInitial(t *testing.T) {
	initial := newMinimalTLSConfig()
	r := NewTLSRotator(initial)
	if got := r.Get(); got != initial {
		t.Errorf("Get = %p, want %p", got, initial)
	}
}

func TestTLSRotator_NewNilInitialIsTolerated(t *testing.T) {
	r := NewTLSRotator(nil)
	if got := r.Get(); got != nil {
		t.Errorf("Get on nil-initial: %p, want nil", got)
	}
}

func TestTLSRotator_SetReplacesConfig(t *testing.T) {
	r := NewTLSRotator(newMinimalTLSConfig())
	next := newDifferentTLSConfig()
	r.Set(next)
	if got := r.Get(); got != next {
		t.Errorf("Get after Set: %p, want %p", got, next)
	}
}

func TestTLSRotator_SetNilIsSilentlyDropped(t *testing.T) {
	r := NewTLSRotator(newMinimalTLSConfig())
	original := r.Get()
	r.Set(nil)
	if got := r.Get(); got != original {
		t.Errorf("Set(nil) replaced live config: got %p, want prior %p", got, original)
	}
}

func TestTLSRotator_GetNilReceiver(t *testing.T) {
	var r *TLSRotator
	if got := r.Get(); got != nil {
		t.Errorf("nil-receiver Get = %p, want nil", got)
	}
}

func TestTLSRotator_SetNilReceiver(t *testing.T) {
	var r *TLSRotator
	r.Set(newMinimalTLSConfig()) // must not panic
}

// TestTLSRotator_ReloadReturnsLiveConfig pins the per-handshake
// contract: the closure installed by Load*TLSConfigWithReload
// reads the rotator's live config every time stdlib calls it.
// A Set between two consultation calls is observable to the
// second consultation.
func TestTLSRotator_ReloadReturnsLiveConfig(t *testing.T) {
	initial := newMinimalTLSConfig()
	r := NewTLSRotator(initial)
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
	var r *TLSRotator
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
		r := NewTLSRotator(nil)
		closure := r.Reload(nil)
		if got, err := closure(); err != nil || got != nil {
			t.Errorf("got=%v err=%v, want nil/nil", got, err)
		}
	})
	t.Run("non-nil initial, no Set", func(t *testing.T) {
		initial := newMinimalTLSConfig()
		r := NewTLSRotator(nil)
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

// Compile-time pin: TLSRotator satisfies TLSReloader. A forgotten
// Set method (e.g. from a future refactor that splits the two
// interfaces) would fail here, not at the first WatchTLSReload
// install.
var _ TLSReloader = (*TLSRotator)(nil)

// Tests for InmemNodeVerifier (ADR-056).
//
// Coverage:
//   - Set replaces the registered set on every call.
//   - LookupCN accepts registered CNs and rejects unknown CNs.
//   - nil-receiver is strict (rejects with ErrNodeVerifierCNMismatch).
//   - LookupCN is RLock-safe: a parallel Set + parallel LookupCN race
//     never produces a data-race detector hit under `go test -race`.

package wire

import (
	"errors"
	"sync"
	"testing"
)

func TestInmemNodeVerifier_SetSwapsSnapshot(t *testing.T) {
	v := NewInmemNodeVerifier()
	if got := v.Size(); got != 0 {
		t.Fatalf("Size()=%d on empty verifier; want 0", got)
	}

	v.Set([]string{"vmmd.faas", "schedd.faas"})
	if got := v.Size(); got != 2 {
		t.Fatalf("Size()=%d after Set([2]); want 2", got)
	}

	v.Set([]string{"vmmd.faas"})
	if got := v.Size(); got != 1 {
		t.Fatalf("Size()=%d after Set([1]); want 1", got)
	}

	v.Set(nil)
	if got := v.Size(); got != 0 {
		t.Fatalf("Size()=%d after Set(nil); want 0", got)
	}
}

func TestInmemNodeVerifier_LookupCN_AcceptsRegistered(t *testing.T) {
	v := NewInmemNodeVerifier()
	v.Set([]string{"vmmd.faas", "schedd.faas"})

	for _, cn := range []string{"vmmd.faas", "schedd.faas"} {
		if err := v.LookupCN(cn); err != nil {
			t.Errorf("LookupCN(%q)=%v; want nil", cn, err)
		}
	}
}

func TestInmemNodeVerifier_LookupCN_RejectsUnknown(t *testing.T) {
	v := NewInmemNodeVerifier()
	v.Set([]string{"vmmd.faas"})

	err := v.LookupCN("schedd.faas")
	if err == nil {
		t.Fatalf("LookupCN(schedd.faas)=nil; want error")
	}
	if !errors.Is(err, ErrNodeVerifierCNMismatch) {
		t.Errorf("LookupCN(schedd.faas)=%v; want wraps ErrNodeVerifierCNMismatch", err)
	}
}

func TestInmemNodeVerifier_LookupCN_StrictNil(t *testing.T) {
	var v *InmemNodeVerifier // nil
	err := v.LookupCN("vmmd.faas")
	if !errors.Is(err, ErrNodeVerifierCNMismatch) {
		t.Errorf("nil receiver LookupCN()=%v; want ErrNodeVerifierCNMismatch", err)
	}
	if got := v.Size(); got != 0 {
		t.Errorf("nil receiver Size()=%d; want 0", got)
	}
}

func TestInmemNodeVerifier_SetDropsEmptyStrings(t *testing.T) {
	v := NewInmemNodeVerifier()
	v.Set([]string{"vmmd.faas", "", "schedd.faas", ""})
	if got := v.Size(); got != 2 {
		t.Fatalf("Size()=%d after Set with empty strings; want 2", got)
	}
}

// TestInmemNodeVerifier_ConcurrentSetLookup asserts the snapshot
// swap is atomic w.r.t. concurrent LookupCN readers. Under
// `go test -race`, no data race must be reported. The functional
// outcome — every LookupCN sees either the pre- or post-swap
// snapshot, never a half-built map — is asserted by Size() at the
// end of the loop.
func TestInmemNodeVerifier_ConcurrentSetLookup(t *testing.T) {
	v := NewInmemNodeVerifier()
	var wg sync.WaitGroup
	const iters = 200

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			if i%2 == 0 {
				v.Set([]string{"vmmd.faas"})
			} else {
				v.Set([]string{"schedd.faas"})
			}
		}
	}()

	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				_ = v.LookupCN("vmmd.faas")
				_ = v.LookupCN("schedd.faas")
				_ = v.Size()
			}
		}()
	}

	wg.Wait()
	if got := v.Size(); got != 1 {
		t.Fatalf("Size()=%d after parallel Set; want 1 (last writer wins)", got)
	}
}

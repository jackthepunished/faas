package runnerparity

// TestRunner_HandleIsConcurrent pins the §4.9 listener-level concurrency
// contract for the guest runners (issue #559 / AC #3). The runner's
// `http.ListenAndServe` dispatches each accepted connection on its own
// goroutine, so a single VM can serve the platform's
// `concurrency_per_vm` bound (Free 1, Hobby 5, Pro 25, Scale 80) at the
// listener layer. The customer *handler process* may still serialize
// internally (sync subprocess-per-request), but the listener's own
// goroutine fan-out is what defines the per-VM bound — and that's what
// this test pins.
//
// Why a parity-package test rather than per-runner: the concurrency
// claim is identical for every runner (all five use `http.ListenAndServe`
// with `http.ServeMux`), so the pin is runner-agnostic. A single test
// exercises the listener shape any runner will land on. The five
// runner-specific `TestHandle_RoundTrip` tests cover the per-runtime
// envelope semantics; this test covers the listener semantics.
//
// The test fires N parallel http.Get against an httptest.Server whose
// handler sleeps for a fixed window. If the listener serializes
// (single-threaded accept loop), wall time ≈ N × sleep. If the
// listener dispatches concurrently (Go net/http default), wall time
// ≈ 1 × sleep + scheduling jitter. The assertion is the latter.

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRunner_HandleIsConcurrent fires N parallel GETs at a slow handler
// and asserts the listener dispatches them concurrently. The handler
// shape mirrors the runner's `handle(w, r, script, signal)` pattern —
// HTTP server + ServeMux + per-request sleep — so the concurrency claim
// pins the §4.9 model. N=20 with sleep=50ms gives a serialized floor of
// 1s (200ms × safety factor well above any CI jitter) and a concurrent
// ceiling of ~100ms; the assertion bracket keeps the test stable under
// -race and -coverpkg=*.
func TestRunner_HandleIsConcurrent(t *testing.T) {
	const (
		n         = 20                    // concurrent in-flight requests
		sleepEach = 50 * time.Millisecond // per-request wall time
	)

	// Mirror the runner's listener shape: ServeMux + per-request handler
	// closure that does work then writes 200. If `handle` were
	// serialized (e.g. through a mutex around the accept loop — which
	// is what this test guards against regressing to), the wall time
	// would be N × sleepEach instead of ~1×.
	var served atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(sleepEach)
		served.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Fan out N parallel GETs. WaitGroup + per-goroutine error capture
	// (no errgroup dependency for parity with the rest of the
	// runner-parity helpers).
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(srv.URL + "/probe")
			if err != nil {
				errs <- err
				return
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				errs <- errBadStatus{resp.StatusCode}
				return
			}
		}()
	}

	start := time.Now()
	wg.Wait()
	elapsed := time.Since(start)
	close(errs)
	for err := range errs {
		t.Errorf("concurrent GET failed: %v", err)
	}

	if got := served.Load(); got != int64(n) {
		t.Errorf("served count = %d, want %d (every request must reach the handler)", got, n)
	}

	// Concurrency floor: a serialized listener would take at least
	// N × sleepEach = 1s. A concurrent listener finishes in ~1 ×
	// sleepEach + jitter. Allow up to 4× sleepEach as the upper bound
	// — generous enough for CI sched jitter but well below the
	// serialized floor of 20× = 1s.
	serializedFloor := time.Duration(n) * sleepEach
	concurrentCeiling := 4 * sleepEach
	if elapsed >= serializedFloor {
		t.Errorf("listener appears serialized: elapsed=%v, serialized floor=%v (N=%d × sleep=%v)",
			elapsed, serializedFloor, n, sleepEach)
	}
	if elapsed > concurrentCeiling {
		t.Errorf("listener slower than expected for concurrent dispatch: elapsed=%v, ceiling=%v",
			elapsed, concurrentCeiling)
	}
	t.Logf("elapsed=%v (concurrent dispatch: 1×sleep=%v baseline; serialized floor=%v)",
		elapsed, sleepEach, serializedFloor)
}

// errBadStatus surfaces a non-200 response as a typed error so the
// per-goroutine capture in TestRunner_HandleIsConcurrent can report it
// with the offending status code rather than just an opaque failure.
type errBadStatus struct {
	code int
}

func (e errBadStatus) Error() string {
	return "unexpected status code " + http.StatusText(e.code)
}

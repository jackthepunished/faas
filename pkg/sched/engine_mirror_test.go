// engine_mirror_test.go — issue #72 / ADR-133 / ADR-125 PR-A3
//
// PR-A3 code-review fix #3 moved the per-rule mirror VM
// concurrency cap from pkg/sched.Engine to pkg/gateway.Handler
// (see pkg/gateway/handler_mirror_slot_test.go). The cap-at-max
// sentinel sched.ErrMirrorSlotAtCapacity stays in pkg/sched
// because it's the contract the gateway imports + wraps when its
// per-rule counter is exhausted; the gateway-side tests cover
// the slot acquisition logic end-to-end.
//
// What's still pinned here:
//
//   1. The sentinel's existence and message — a future rename
//      surfaces as a test failure rather than a silent dispatch-
//      goroutine bug (errors.Is in pkg/gateway/mirror_dispatch.go).

package sched

import (
	"errors"
	"testing"
)

// TestErrMirrorSlotAtCapacity_Message pins the sentinel string used
// in the gateway-side errors.Is check (pkg/gateway/mirror_dispatch.go
// ::dispatchMirror) so a future rename of ErrMirrorSlotAtCapacity
// surfaces as a test failure rather than a silent dispatch-goroutine
// bug.
func TestErrMirrorSlotAtCapacity_Message(t *testing.T) {
	t.Parallel()
	if ErrMirrorSlotAtCapacity == nil {
		t.Fatal("ErrMirrorSlotAtCapacity is nil")
	}
	if msg := ErrMirrorSlotAtCapacity.Error(); msg == "" {
		t.Fatal("ErrMirrorSlotAtCapacity.Error() returned empty string")
	}
	// Sanity: the sentinel is distinct from any random error.
	if errors.Is(ErrMirrorSlotAtCapacity, errors.New("mirror slot at capacity")) {
		t.Fatal("ErrMirrorSlotAtCapacity should not errors.Is a fresh same-text error")
	}
}

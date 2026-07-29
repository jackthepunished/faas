// stateless_advisory_test.go — Wave 0 PR-C / ADR-047 unit tests.
//
// These tests pin the Manager.ForwardStatelessAdvisory seam: empty
// batches short-circuit, nil-client (default-local) silently drops,
// and the AdvisoryForwarder receives the parsed batch verbatim.
//
// ADR-035: the assertion is "advisory row written OR dropped with
// Warn", not "advisory row written". A dropped advisory is the
// correct outcome on a down apid, so the tests do not assert
// success on the apid side — they assert the Manager's contract.

package fcvm

import (
	"context"
	"sync"
	"testing"
)

// stubAdvisoryForwarder is the in-test AdvisoryForwarder. Records
// every call so tests can assert what the Manager handed off.
type stubAdvisoryForwarder struct {
	mu    sync.Mutex
	calls []stubAdvisoryCall
	// failErr, when non-nil, is returned from Forward. Manager
	// must swallow the error (ADR-035: drop on forward failure).
	failErr error
}

type stubAdvisoryCall struct {
	Instance string
	AppID    string
	Batch    []AdvisoryEvent
}

func (s *stubAdvisoryForwarder) Forward(_ context.Context, instance, appID string, events []AdvisoryEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, stubAdvisoryCall{Instance: instance, AppID: appID, Batch: events})
	return s.failErr
}

func (s *stubAdvisoryForwarder) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// TestForwardStatelessAdvisory_AppendsRow asserts the happy path:
// the Manager hands the parsed batch to the AdvisoryForwarder
// verbatim, and instance+appID survive intact.
func TestForwardStatelessAdvisory_AppendsRow(t *testing.T) {
	stub := &stubAdvisoryForwarder{}
	mgr := NewManager(&fakeRunner{}, &fakeVMM{}, Paths{Kernel: "/k"}, "1.7.0", nil, nil)
	mgr.SetAdvisoryClient(stub)

	batch := []AdvisoryEvent{
		{Path: "/data/x", Masks: []string{"create"}, PID: 42, TsUnix: 1700000000000},
		{Path: "/data/y", Masks: []string{"modify"}, PID: 42, TsUnix: 1700000001000},
	}
	if err := mgr.ForwardStatelessAdvisory(context.Background(), "i-test", "a-test", batch); err != nil {
		t.Fatalf("ForwardStatelessAdvisory: %v", err)
	}
	if got := stub.callCount(); got != 1 {
		t.Fatalf("Forward call count = %d, want 1", got)
	}
	calls := stub.calls // safe: only one call
	if calls[0].Instance != "i-test" || calls[0].AppID != "a-test" {
		t.Errorf("Forward call instance/app = %q/%q, want i-test/a-test", calls[0].Instance, calls[0].AppID)
	}
	if got := len(calls[0].Batch); got != 2 {
		t.Errorf("Forward batch size = %d, want 2", got)
	}
}

// TestForwardStatelessAdvisory_NilClientNoPanic is the default-local
// safety net. A Manager built without SetAdvisoryClient (which is
// the vmmd unit-test posture) must not panic and must return nil.
func TestForwardStatelessAdvisory_NilClientNoPanic(t *testing.T) {
	mgr := NewManager(&fakeRunner{}, &fakeVMM{}, Paths{Kernel: "/k"}, "1.7.0", nil, nil)
	// Note: no SetAdvisoryClient call.
	batch := []AdvisoryEvent{{Path: "/data/x", Masks: []string{"create"}}}
	if err := mgr.ForwardStatelessAdvisory(context.Background(), "i", "a", batch); err != nil {
		t.Fatalf("ForwardStatelessAdvisory on nil client: %v", err)
	}
}

// TestForwardStatelessAdvisory_EmptyBatchNoEmit asserts the
// debounce-to-empty edge case: a fanotify storm that drains to zero
// events within the window must not pollute the audit table.
func TestForwardStatelessAdvisory_EmptyBatchNoEmit(t *testing.T) {
	stub := &stubAdvisoryForwarder{}
	mgr := NewManager(&fakeRunner{}, &fakeVMM{}, Paths{Kernel: "/k"}, "1.7.0", nil, nil)
	mgr.SetAdvisoryClient(stub)

	if err := mgr.ForwardStatelessAdvisory(context.Background(), "i", "a", nil); err != nil {
		t.Fatalf("ForwardStatelessAdvisory nil batch: %v", err)
	}
	if err := mgr.ForwardStatelessAdvisory(context.Background(), "i", "a", []AdvisoryEvent{}); err != nil {
		t.Fatalf("ForwardStatelessAdvisory empty batch: %v", err)
	}
	if got := stub.callCount(); got != 0 {
		t.Errorf("Forward call count on empty batches = %d, want 0", got)
	}
}

// TestForwardStatelessAdvisory_ClientErrorSwallowed pins ADR-035:
// the AdvisoryForwarder returned an error (e.g. apid is down). The
// Manager must NOT bubble that up; the advisory is observation, not
// source of truth, and a noisy log is the worst the customer sees.
func TestForwardStatelessAdvisory_ClientErrorSwallowed(t *testing.T) {
	stub := &stubAdvisoryForwarder{failErr: errAdvisoryStubFailed}
	mgr := NewManager(&fakeRunner{}, &fakeVMM{}, Paths{Kernel: "/k"}, "1.7.0", nil, nil)
	mgr.SetAdvisoryClient(stub)

	batch := []AdvisoryEvent{{Path: "/data/x", Masks: []string{"create"}}}
	if err := mgr.ForwardStatelessAdvisory(context.Background(), "i", "a", batch); err != nil {
		t.Fatalf("Manager must swallow AdvisoryForwarder errors, got %v", err)
	}
	if got := stub.callCount(); got != 1 {
		t.Errorf("Forward was called %d times, want 1 (single forward, error swallowed)", got)
	}
}

// errAdvisoryStubFailed is a sentinel used by the stub failure test
// only. Distinct from any real package error so a future
// refactor that surfaces it accidentally trips the test.
var errAdvisoryStubFailed = &stubAdvisoryError{msg: "stub: forward failed"}

type stubAdvisoryError struct{ msg string }

func (e *stubAdvisoryError) Error() string { return e.msg }

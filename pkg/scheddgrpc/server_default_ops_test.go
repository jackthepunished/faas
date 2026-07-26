// White-box test for the `ops == nil` default branch in Server.New
// (server.go:62-64). The bufconn/client tests always pass a
// non-nil wire.OpsMetrics so they don't exercise the default.
// This file is in `package scheddgrpc` so it can call New
// directly with a nil ops — the only way to verify the
// production-side fallback to wire.NewOpsMetrics("schedd").
package scheddgrpc

import (
	"context"
	"testing"

	"github.com/onebox-faas/faas/pkg/sched"
	"github.com/onebox-faas/faas/pkg/state"
)

// noopEngine is a SchedAPI implementation that returns zero values
// for every method. Lives next to the test that uses it (the
// bufconn tests use their own fakeEngine in the `scheddgrpc_test`
// package; this one is for in-package white-box tests).
type noopEngine struct{}

func (noopEngine) Wake(context.Context, string) (sched.WakeResult, error) {
	return sched.WakeResult{}, nil
}
func (noopEngine) AdmitInstance(context.Context, string) (sched.WakeResult, error) {
	return sched.WakeResult{}, nil
}
func (noopEngine) ReportActivity(context.Context, []state.InstanceTouch) (int, error) {
	return 0, nil
}
func (noopEngine) ParkWithReason(context.Context, string, string) error { return nil }

// TestServerNew_NilOpsUsesDefault confirms the
// "ops == nil → wire.NewOpsMetrics(\"schedd\")" fallback
// (server.go:62-64). The constructor must not panic on nil ops
// because ad-hoc test harnesses and the /test/throwaway scripts
// sometimes don't carry a metrics registry. A panic here would
// kill a daemon boot.
func TestServerNew_NilOpsUsesDefault(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("New panicked with nil ops: %v", r)
		}
	}()
	s := New(noopEngine{}, nil, nil)
	if s == nil {
		t.Fatal("New returned nil Server")
	}
	if s.ops == nil {
		t.Fatal("ops not defaulted; the nil-check branch is unreachable")
	}
}

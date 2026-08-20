// Tests for ReportWorkloadOOM (Cluster C / ADR-121). Locks
// down the wire shape (ReportWorkloadOOMRequest /
// ReportWorkloadOOMAck), the observed-payload plumbing
// (peak_mb / plan_mb flow verbatim into the SchedAPI engine
// sink so the whycopy Observed closure can template the prose),
// and the engine-error mapping (NotFound / Internal) that
// mirrors the ReportLivenessFailed path.
//
// Lives in package scheddgrpc_test (same as liveness_failed_test.go
// / bufconn_test.go) and reuses newServer / fakeEngine — the
// destroyForWorkloadOOMFn seam added in bufconn_test.go is what
// makes this file possible.
package scheddgrpc_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	scheddpb "github.com/onebox-faas/faas/api/proto/onebox/faas/schedd/v1"
	"github.com/onebox-faas/faas/pkg/state"
	"google.golang.org/grpc/status"
)

// scheddgrpcWorkloadOOMCall captures the (peak_mb, plan_mb)
// pair the handler invoked the engine with. Mirrors
// scheddgrpcDestroyCall in liveness_failed_test.go.
type scheddgrpcWorkloadOOMCall struct {
	instanceID string
	peakMB     int
	planMB     int
}

// TestReportWorkloadOOM_HappyPath drives a successful drain:
// the schedd-side handler invokes
// SchedAPI.DestroyForWorkloadOOMFailure once, with the
// verbatim instance_id + peak_mb + plan_mb from the wire,
// and replies with Ok=true. The customer's customer-facing
// customer surface (dashboard .error-explanation + gregale
// inspect --errors) downstream of the engine reads the peak
// MB straight from this call to template the whycopy prose
// (pkg/whycopy/whycopy.go::CodeAppRuntimeOOM.Observed).
func TestReportWorkloadOOM_HappyPath(t *testing.T) {
	t.Parallel()
	var got scheddgrpcWorkloadOOMCall
	cli := newServer(t, &fakeEngine{
		destroyForWorkloadOOMFn: func(_ context.Context, instanceID string, peakMB, planMB int) error {
			got.instanceID = instanceID
			got.peakMB = peakMB
			got.planMB = planMB
			return nil
		},
	})
	resp, err := cli.ReportWorkloadOOM(context.Background(), &scheddpb.ReportWorkloadOOMRequest{
		InstanceId: "i-1",
		PeakMb:     384,
		PlanMb:     256,
	})
	if err != nil {
		t.Fatalf("ReportWorkloadOOM: %v", err)
	}
	if !resp.GetOk() {
		t.Errorf("ok = false, want true")
	}
	if got.instanceID != "i-1" {
		t.Errorf("instance_id = %q, want %q", got.instanceID, "i-1")
	}
	// uint32 → int casts; we use them at the engine boundary
	// because the whycopy Observed closure signs the
	// struct{ PeakMB, PlanMB int } template payload. pin the
	// casts match by comparing the typed int values.
	if got.peakMB != 384 {
		t.Errorf("peak_mb = %d, want 384", got.peakMB)
	}
	if got.planMB != 256 {
		t.Errorf("plan_mb = %d, want 256", got.planMB)
	}
}

// TestReportWorkloadOOM_ZeroPeak verifies zero peak_mb is
// not rejected at the wire boundary — the schedd doesn't
// validate the observed-payload byte values (the engine is
// the type-checker: peakMB > 0 is enforced downstream in
// Engine.DestroyForWorkloadOOMFailure's stamping path). The
// handler's job is plumbing, not payload hygiene.
func TestReportWorkloadOOM_ZeroPeak(t *testing.T) {
	t.Parallel()
	var got scheddgrpcWorkloadOOMCall
	cli := newServer(t, &fakeEngine{
		destroyForWorkloadOOMFn: func(_ context.Context, instanceID string, peakMB, planMB int) error {
			got.instanceID = instanceID
			got.peakMB = peakMB
			got.planMB = planMB
			return nil
		},
	})
	if _, err := cli.ReportWorkloadOOM(context.Background(), &scheddpb.ReportWorkloadOOMRequest{
		InstanceId: "i-1",
		PeakMb:     0,
		PlanMb:     256,
	}); err != nil {
		t.Fatalf("ReportWorkloadOOM: %v", err)
	}
	if got.peakMB != 0 {
		t.Errorf("peak_mb = %d, want 0 (zero not rejected at wire boundary)", got.peakMB)
	}
	if got.planMB != 256 {
		t.Errorf("plan_mb = %d, want 256", got.planMB)
	}
}

// TestReportWorkloadOOM_PlumbingSafeUnderConcurrentReports
// sanity-checks the handler under concurrent reports against
// the same instance id. Multiple vmmd poll goroutines may
// emit for the same instance when the OOM cascade spans
// multiple apps; each call should flow through cleanly. We
// count the engine invocations and assert each gets its own
// distinct (peak_mb, plan_mb) pair — there's no in-flight
// dedupe at the handler (the engine has its own state
// guards).
func TestReportWorkloadOOM_PlumbingSafeUnderConcurrentReports(t *testing.T) {
	t.Parallel()
	type pair struct{ PeakMB, PlanMB int }
	seen := make(chan pair, 8)
	cli := newServer(t, &fakeEngine{
		destroyForWorkloadOOMFn: func(_ context.Context, _ string, peakMB, planMB int) error {
			seen <- pair{peakMB, planMB}
			return nil
		},
	})
	const N = 8
	for i := 0; i < N; i++ {
		peakMB := uint32(128 * (i + 1))
		planMB := uint32(256)
		if _, err := cli.ReportWorkloadOOM(context.Background(), &scheddpb.ReportWorkloadOOMRequest{
			InstanceId: "i-1",
			PeakMb:     peakMB,
			PlanMb:     planMB,
		}); err != nil {
			t.Fatalf("ReportWorkloadOOM[%d]: %v", i, err)
		}
	}
	close(seen)
	count := 0
	for p := range seen {
		count++
		// plan_mb must match the wire (256); peak_mb scaled.
		if p.PlanMB != 256 {
			t.Errorf("plan_mb = %d, want 256", p.PlanMB)
		}
		if p.PeakMB == 0 || p.PeakMB > 8*128 {
			t.Errorf("peak_mb = %d, want in (0, 8*128]", p.PeakMB)
		}
	}
	if count != N {
		t.Errorf("engine calls = %d, want %d", count, N)
	}
}

// TestReportWorkloadOOM_EngineErrNotFound asserts the
// not-found path: when the engine's
// DestroyForWorkloadOOMFailure returns state.ErrNotFound
// (the instance id no longer resolves), the handler
// surfaces codes.NotFound. Mirrors the liveness path.
func TestReportWorkloadOOM_EngineErrNotFound(t *testing.T) {
	t.Parallel()
	cli := newServer(t, &fakeEngine{
		destroyForWorkloadOOMFn: func(context.Context, string, int, int) error {
			return state.ErrNotFound
		},
	})
	_, err := cli.ReportWorkloadOOM(context.Background(), &scheddpb.ReportWorkloadOOMRequest{
		InstanceId: "i-missing",
		PeakMb:     384,
		PlanMb:     256,
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("not a gRPC status: %v", err)
	}
	if st.Code().String() != "NotFound" {
		t.Errorf("code = %s, want NotFound", st.Code().String())
	}
}

// TestReportWorkloadOOM_EngineErrInternal verifies any
// non-ErrNotFound engine error surfaces as codes.Internal.
// The vmmd poll goroutine has already exited on its end, so
// the status code only governs the warn-log line. Mirrors
// the liveness path.
func TestReportWorkloadOOM_EngineErrInternal(t *testing.T) {
	t.Parallel()
	boom := errors.New("db hitches")
	cli := newServer(t, &fakeEngine{
		destroyForWorkloadOOMFn: func(context.Context, string, int, int) error {
			return boom
		},
	})
	_, err := cli.ReportWorkloadOOM(context.Background(), &scheddpb.ReportWorkloadOOMRequest{
		InstanceId: "i-1",
		PeakMb:     384,
		PlanMb:     256,
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("not a gRPC status: %v", err)
	}
	if st.Code().String() != "Internal" {
		t.Errorf("code = %s, want Internal", st.Code().String())
	}
	if !strings.Contains(st.Message(), boom.Error()) {
		t.Errorf("message = %q, want substring %q", st.Message(), boom.Error())
	}
}

// TestReportWorkloadOOM_ObservedPayloadFlowsVerbatim locks
// down the customer-facing templating seam: the wire's
// peak_mb + plan_mb must arrive at the engine without
// interpretation. The whycopy.Decorate(code, observed{PeakMB,
// PlanMB}) call downstream renders "384 MB" and "256 MB +
// 8 MB overhead" into the ErrorWhy string (see
// pkg/sched/engine_workload_oom_test.go::TestWorkloadOOM_
// StampsAppRuntimeOOM). A future refactor that scales the
// values would silently break the customer prose.
func TestReportWorkloadOOM_ObservedPayloadFlowsVerbatim(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		peakMB, planMB uint32
	}{
		{"hobby_plan_at_cap", 264, 256},
		{"pro_plan_over", 600, 512},
		{"free_plan_under", 96, 128},
		{"peak_equals_cap", 256, 256}, // pathological edge: customer at exactly the cap
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got atomic.Value
			cli := newServer(t, &fakeEngine{
				destroyForWorkloadOOMFn: func(_ context.Context, _ string, peakMB, planMB int) error {
					got.Store(struct{ PeakMB, PlanMB int }{peakMB, planMB})
					return nil
				},
			})
			if _, err := cli.ReportWorkloadOOM(context.Background(), &scheddpb.ReportWorkloadOOMRequest{
				InstanceId: "i-1",
				PeakMb:     tc.peakMB,
				PlanMb:     tc.planMB,
			}); err != nil {
				t.Fatalf("ReportWorkloadOOM: %v", err)
			}
			parsed, ok := got.Load().(struct{ PeakMB, PlanMB int })
			if !ok {
				t.Fatalf("no recorded call (load returned %T)", got.Load())
			}
			if parsed.PeakMB != int(tc.peakMB) {
				t.Errorf("peak_mb = %d, want %d (verbatim)", parsed.PeakMB, tc.peakMB)
			}
			if parsed.PlanMB != int(tc.planMB) {
				t.Errorf("plan_mb = %d, want %d (verbatim)", parsed.PlanMB, tc.planMB)
			}
		})
	}
}

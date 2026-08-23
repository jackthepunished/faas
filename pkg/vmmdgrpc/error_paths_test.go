// error_paths_test.go — branch coverage for the gRPC error-code
// mappings in pkg/vmmdgrpc/migration_handlers.go left by
// migration_handlers_test.go.
//
// migration_handlers_test.go covers:
//   - PrepareLiveMigration/missing-fields → codes.InvalidArgument
//   - AdoptMigratedInstance/missing-fields → codes.InvalidArgument
//   - AdoptMigratedInstance/lease-lookup-failed → codes.NotFound
//     (the errLeaseNotFound branch)
//   - AcknowledgeMigration/missing-fields → codes.InvalidArgument
//   - CancelLiveMigration/missing-fields → codes.InvalidArgument
//
// What this file adds:
//   - AdoptMigratedInstance lease-token mismatch → codes.PermissionDenied
//     (the errLeaseMismatch branch in `errors.As(err, &lm)`)
//   - AdoptMigratedInstance valid lease → success response (no error)
//   - AcknowledgeMigration happy-path → tracker delete branch
//   - AcknowledgeMigration stale idempotent → getErr != nil absorbed
//   - CancelLiveMigration happy-path → tracker delete branch
//   - CancelLiveMigration stale idempotent → getErr != nil absorbed
//
// Why no PrepareLiveMigration duplicate-already-exists case:
// that path requires s.vmm.Park(ctx, ...) to succeed first.
// s.vmm is *fcvm.Manager (not an interface), so we can't stub
// Park without root + KVM (covered end-to-end in
// bufconn_test.go::TestPrepareLiveMigration_DuplicateAlreadyExists).
//
// Whitebox test (package vmmdgrpc).
package vmmdgrpc

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	vmmdpb "github.com/onebox-faas/faas/api/proto/onebox/faas/vmmd/v1"
	"github.com/onebox-faas/faas/pkg/grpcerr"
)

// TestAdoptMigratedInstance_LeaseMismatch pins the
// `errors.As(err, &lm)` branch in AdoptMigratedInstance. A
// lease exists for the instance but a wrong token was supplied;
// the handler must surface codes.PermissionDenied (NOT
// codes.NotFound) — the canonical "lease token mismatch"
// branch.
func TestAdoptMigratedInstance_LeaseMismatch(t *testing.T) {
	s := newMigrationHandlersTestServer(t)

	// Seed an active lease with the right token.
	if err := s.migrations.put(&activeMigration{
		instanceID: "iid-mismatch",
		leaseToken: "right",
	}); err != nil {
		t.Fatalf("seed put: %v", err)
	}

	// Adopt with a wrong token — must hit the errLeaseMismatch branch.
	_, err := s.AdoptMigratedInstance(context.Background(), &vmmdpb.AdoptMigratedInstanceRequest{
		InstanceId:        "iid-mismatch",
		MemStorageKey:     "snap/mem",
		VmstateStorageKey: "snap/vmstate",
		LeaseToken:        "WRONG",
	})
	if err == nil {
		t.Fatal("AdoptMigratedInstance(wrong token): want err, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("status.FromError: %v", err)
	}
	if st.Code() != codes.PermissionDenied {
		t.Errorf("Code = %v, want codes.PermissionDenied (lease-token mismatch branch)", st.Code())
	}
	// Verify the wrapping preserves the original errLeaseMismatch
	// (errors.Is would also work but the handler does not wrap via
	// %w; instead, the surfaced message contains the underlying
	// context).
	var lm errLeaseMismatch
	if !errors.As(err, &lm) && !contains(st.Message(), "lease token mismatch") {
		t.Errorf("err = %v; want errLeaseMismatch (or message containing 'lease token mismatch')", err)
	}
}

// TestAdoptMigratedInstance_LeaseValid pins the happy path:
// a correctly-presented lease returns an empty response with
// no error. The handler does NOT touch the tracker (only the
// stale-acquire path would); this test pins both the no-error
// return AND that the tracker retains the lease for Phase 5
// AcknowledgeMigration to find.
func TestAdoptMigratedInstance_LeaseValid(t *testing.T) {
	s := newMigrationHandlersTestServer(t)

	if err := s.migrations.put(&activeMigration{
		instanceID: "iid-happy",
		leaseToken: "tok",
	}); err != nil {
		t.Fatalf("seed put: %v", err)
	}

	resp, err := s.AdoptMigratedInstance(context.Background(), &vmmdpb.AdoptMigratedInstanceRequest{
		InstanceId:        "iid-happy",
		MemStorageKey:     "snap/mem",
		VmstateStorageKey: "snap/vmstate",
		LeaseToken:        "tok",
	})
	if err != nil {
		t.Fatalf("valid adopt: %v", err)
	}
	if resp == nil {
		t.Fatal("resp = nil on happy path, want empty response")
	}
	// Lease still present (handler does not delete on adopt — the
	// ack/cancel phases do that). Whitebox access to the
	// tracker's state map; the lock semantics are owned by
	// put/get/delete.
	if _, ok := s.migrations.state["iid-happy"]; !ok {
		t.Error("lease disappeared after Adopt (Phase 5's job, not Phase 3's)")
	}
}

// TestAcknowledgeMigration_SuccessDelete pins the happy path:
// the lease is present and the handler deletes it. Returns
// success + no error. Pinned because this is the Phase 5
// "commit" path — losing the delete would leak the lease
// forever and cause repeated Phase 4 ack attempts to fire.
func TestAcknowledgeMigration_SuccessDelete(t *testing.T) {
	s := newMigrationHandlersTestServer(t)
	if err := s.migrations.put(&activeMigration{
		instanceID: "iid-ack",
		leaseToken: "tok",
	}); err != nil {
		t.Fatalf("seed put: %v", err)
	}

	resp, err := s.AcknowledgeMigration(context.Background(), &vmmdpb.AcknowledgeMigrationRequest{
		InstanceId: "iid-ack",
		LeaseToken: "tok",
	})
	if err != nil {
		t.Fatalf("ack: %v", err)
	}
	if resp == nil {
		t.Fatal("resp = nil on happy path, want empty response")
	}
	// Lease MUST be deleted.
	if _, ok := s.migrations.state["iid-ack"]; ok {
		t.Error("lease still present after AcknowledgeMigration (Phase 5 leak)")
	}
}

// TestAcknowledgeMigration_StaleIdempotent pins the
// `getErr != nil` absorption branch: a re-sent ack on a stale
// lease MUST NOT surface an error. The canonical wire contract
// for "already cleared" is "idempotent success" — pinning this
// prevents a future refactor from leaking the error to the
// orchestrator and causing Phase-5 retries to surface as
// spurious failures.
func TestAcknowledgeMigration_StaleIdempotent(t *testing.T) {
	s := newMigrationHandlersTestServer(t)
	// No prior lease; the ack finds nothing, getErr != nil,
	// handler returns nil err.
	resp, err := s.AcknowledgeMigration(context.Background(), &vmmdpb.AcknowledgeMigrationRequest{
		InstanceId: "iid-stale",
		LeaseToken: "tok",
	})
	if err != nil {
		t.Errorf("stale ack: want nil err (idempotent), got %v", err)
	}
	if resp == nil {
		t.Error("resp = nil on stale path, want empty response")
	}
}

// TestCancelLiveMigration_SuccessDelete pins Phase 4's
// happy-path: lease is present, handler deletes it, returns
// success. Symmetric to Acknowledge.
func TestCancelLiveMigration_SuccessDelete(t *testing.T) {
	s := newMigrationHandlersTestServer(t)
	if err := s.migrations.put(&activeMigration{
		instanceID: "iid-cancel",
		leaseToken: "tok",
	}); err != nil {
		t.Fatalf("seed put: %v", err)
	}
	resp, err := s.CancelLiveMigration(context.Background(), &vmmdpb.CancelLiveMigrationRequest{
		InstanceId: "iid-cancel",
		LeaseToken: "tok",
	})
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if resp == nil {
		t.Fatal("resp = nil on happy path, want empty response")
	}
	if _, ok := s.migrations.state["iid-cancel"]; ok {
		t.Error("lease still present after Cancel (Phase 4 leak)")
	}
}

// TestCancelLiveMigration_StaleIdempotent pins the
// `getErr != nil` absorption branch — symmetric to the
// AcknowledgeMigration stale test. A re-sent cancel on a
// stale lease is a no-op success.
func TestCancelLiveMigration_StaleIdempotent(t *testing.T) {
	s := newMigrationHandlersTestServer(t)
	resp, err := s.CancelLiveMigration(context.Background(), &vmmdpb.CancelLiveMigrationRequest{
		InstanceId: "iid-stale-cancel",
		LeaseToken: "tok",
	})
	if err != nil {
		t.Errorf("stale cancel: want nil err (idempotent), got %v", err)
	}
	if resp == nil {
		t.Error("resp = nil on stale path, want empty response")
	}
}

// TestAdoptMigratedInstance_WrongTokenStillDistinctFromMissing
// pins the contract: lease-token-mismatch (codes.PermissionDenied)
// and lease-missing (codes.NotFound) are distinct wire codes. A
// future refactor that collapses them onto a single code trips
// here.
func TestAdoptMigratedInstance_WrongTokenStillDistinctFromMissing(t *testing.T) {
	s := newMigrationHandlersTestServer(t)

	// Case A: wrong token on an existing lease.
	if err := s.migrations.put(&activeMigration{
		instanceID: "iid-A", leaseToken: "right-A",
	}); err != nil {
		t.Fatalf("seed A: %v", err)
	}
	_, errA := s.AdoptMigratedInstance(context.Background(), &vmmdpb.AdoptMigratedInstanceRequest{
		InstanceId:        "iid-A",
		MemStorageKey:     "snap/mem",
		VmstateStorageKey: "snap/vmstate",
		LeaseToken:        "WRONG-A",
	})
	stA, ok := status.FromError(errA)
	if !ok {
		t.Fatalf("Case A: status.FromError: %v", errA)
	}

	// Case B: lease never existed.
	_, errB := s.AdoptMigratedInstance(context.Background(), &vmmdpb.AdoptMigratedInstanceRequest{
		InstanceId:        "iid-B-NEVER",
		MemStorageKey:     "snap/mem",
		VmstateStorageKey: "snap/vmstate",
		LeaseToken:        "tok-B",
	})
	stB, ok := status.FromError(errB)
	if !ok {
		t.Fatalf("Case B: status.FromError: %v", errB)
	}

	// Both errors must be 4xx-shaped AND distinct codes.
	if stA.Code() != codes.PermissionDenied {
		t.Errorf("Case A Code = %v, want codes.PermissionDenied", stA.Code())
	}
	if stB.Code() != codes.NotFound {
		t.Errorf("Case B Code = %v, want codes.NotFound", stB.Code())
	}
	if stA.Code() == stB.Code() {
		t.Errorf("codes collapsed: wrong-token (%v) and missing-lease (%v) must be distinct",
			stA.Code(), stB.Code())
	}
}

// TestAdoptMigratedInstance_DocsURLPreserved pins that ALL
// error codes round-trip through grpcerr with a docs_url. The
// migration_handlers_test.go table covers InvalidArgument +
// NotFound; this pins the PermissionDenied branch's docs_url
// shape (review finding for #420 was "every site must surface
// wire.DocsHost", and the errLeaseMismatch branch is the most
// likely to be forgotten).
func TestAdoptMigratedInstance_DocsURLPreserved(t *testing.T) {
	s := newMigrationHandlersTestServer(t)
	if err := s.migrations.put(&activeMigration{
		instanceID: "iid-url", leaseToken: "right",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err := s.AdoptMigratedInstance(context.Background(), &vmmdpb.AdoptMigratedInstanceRequest{
		InstanceId:        "iid-url",
		MemStorageKey:     "snap/mem",
		VmstateStorageKey: "snap/vmstate",
		LeaseToken:        "WRONG",
	})
	if err == nil {
		t.Fatal("want err, got nil")
	}
	p, ok := grpcerr.FromStatus(err)
	if !ok {
		t.Fatalf("grpcerr.FromStatus: %v", err)
	}
	if p.DocsURL == "" {
		t.Fatal("PermissionDenied branch lost docs_url (issue #420 contract)")
	}
	// The docs_url fragment matches the one in migration_handlers.go
	// (line 290-291: "adopt"). Any future edit to the
	// WithDocs(...) argument trips the prefix check below.
	if !contains(p.DocsURL, "vmmd#adopt") {
		t.Errorf("docs_url = %q; want fragment 'vmmd#adopt' (issue #420)", p.DocsURL)
	}
}

// --- helpers ---

// contains is a tiny strings.Contains replacement.
func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// bufconn_mega4_test.go — Coverage Mega-PR #4 cluster 6 (part 2):
// drive the bufconn-based handler tests for the gRPC entry points
// that forward_server_mega4_test.go (whitebox) cannot reach because
// fakeVMM + newServer live in this blackbox test package.
//
// Targets:
//   - FrameworkReady (empty instance, success, not-live, manager-error)
//   - UpdateEgressAllowlist (success, empty app_id, invalid prefix,
//     manager-error)
//   - MaterializeParentExt4 (success; error path is not reachable
//     because the fakeVMM MaterializeParentExt4 stub returns nil
//     unconditionally — ADR-053 cross-process test exercises the
//     real path)
//
// Blackbox `package vmmdgrpc_test`. Reuses fakeVMM + newServer from
// bufconn_test.go.

package vmmdgrpc_test

import (
	"context"
	"net/netip"
	"runtime"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"

	vmmdpb "github.com/onebox-faas/faas/api/proto/onebox/faas/vmmd/v1"
	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/sched"
)

// --- FrameworkReady ----------------------------------------------

func TestServer_FrameworkReady_Success_Mega4(t *testing.T) {
	t.Parallel()
	f := &fakeVMM{}
	cli, _ := newServer(t, f)
	resp, err := cli.FrameworkReady(context.Background(), &vmmdpb.FrameworkReadyRequest{
		Instance: "i-1", WarmupMs: 250,
	})
	if err != nil {
		t.Fatalf("FrameworkReady: %v", err)
	}
	if resp == nil {
		t.Error("nil response")
	}
}

func TestServer_FrameworkReady_EmptyInstance_Mega4(t *testing.T) {
	t.Parallel()
	f := &fakeVMM{}
	cli, _ := newServer(t, f)
	_, err := cli.FrameworkReady(context.Background(), &vmmdpb.FrameworkReadyRequest{
		Instance: "", WarmupMs: 100,
	})
	if err == nil {
		t.Fatal("want err for empty instance")
	}
	if !strings.Contains(err.Error(), "Invalid") {
		t.Errorf("err = %v, want InvalidArgument", err)
	}
}

func TestServer_FrameworkReady_NotLive_Mega4(t *testing.T) {
	t.Parallel()
	f := &fakeVMM{
		frameworkReadyFn: func(_ context.Context, _ string, _ int64) (bool, string, string, error) {
			return false, "", "", nil
		},
	}
	cli, _ := newServer(t, f)
	_, err := cli.FrameworkReady(context.Background(), &vmmdpb.FrameworkReadyRequest{
		Instance: "i-1", WarmupMs: 100,
	})
	if err == nil {
		t.Fatal("want err for non-stamped")
	}
	if !strings.Contains(err.Error(), "NotFound") {
		t.Errorf("err = %v, want NotFound", err)
	}
}

func TestServer_FrameworkReady_ManagerError_Mega4(t *testing.T) {
	t.Parallel()
	f := &fakeVMM{
		frameworkReadyFn: func(_ context.Context, _ string, _ int64) (bool, string, string, error) {
			return false, "", "", errFake
		},
	}
	cli, _ := newServer(t, f)
	_, err := cli.FrameworkReady(context.Background(), &vmmdpb.FrameworkReadyRequest{
		Instance: "i-1", WarmupMs: 100,
	})
	if err == nil {
		t.Fatal("want err")
	}
}

// --- UpdateEgressAllowlist --------------------------------------

func TestServer_UpdateEgressAllowlist_Success_Mega4(t *testing.T) {
	t.Parallel()
	f := &fakeVMM{}
	cli, _ := newServer(t, f)
	_, err := cli.UpdateEgressAllowlist(context.Background(), &vmmdpb.UpdateEgressAllowlistRequest{
		AppId:           "app-1",
		EgressAllowlist: []string{"10.0.0.0/8", "192.168.0.0/16"},
	})
	if err != nil {
		t.Fatalf("UpdateEgressAllowlist: %v", err)
	}
}

func TestServer_UpdateEgressAllowlist_EmptyAppID_Mega4(t *testing.T) {
	t.Parallel()
	f := &fakeVMM{}
	cli, _ := newServer(t, f)
	_, err := cli.UpdateEgressAllowlist(context.Background(), &vmmdpb.UpdateEgressAllowlistRequest{
		AppId: "",
	})
	if err == nil {
		t.Fatal("want err for empty app_id")
	}
}

func TestServer_UpdateEgressAllowlist_InvalidPrefix_Mega4(t *testing.T) {
	t.Parallel()
	f := &fakeVMM{}
	cli, _ := newServer(t, f)
	_, err := cli.UpdateEgressAllowlist(context.Background(), &vmmdpb.UpdateEgressAllowlistRequest{
		AppId:           "app-1",
		EgressAllowlist: []string{"not-a-cidr"},
	})
	if err == nil {
		t.Fatal("want err for invalid prefix")
	}
}

func TestServer_UpdateEgressAllowlist_ManagerError_Mega4(t *testing.T) {
	t.Parallel()
	f := &fakeVMM{
		updateAllowlistFn: func(_ context.Context, _ string, _ []netip.Prefix) error {
			return errFake
		},
	}
	cli, _ := newServer(t, f)
	_, err := cli.UpdateEgressAllowlist(context.Background(), &vmmdpb.UpdateEgressAllowlistRequest{
		AppId:           "app-1",
		EgressAllowlist: []string{"10.0.0.0/8"},
	})
	if err == nil {
		t.Fatal("want err")
	}
}

// --- MaterializeParentExt4 ---------------------------------------

func TestServer_MaterializeParentExt4_Success_Mega4(t *testing.T) {
	t.Parallel()
	f := &fakeVMM{}
	cli, _ := newServer(t, f)
	// Must use a parent-base ext4 key from sched.parentBaseKeyAliases
	// — anything else is rejected by the allow-list pre-check.
	parentKey := sched.BaseKeyForArch(sched.ParentBaseRuntime, runtime.GOARCH)
	_, err := cli.MaterializeParentExt4(context.Background(), &vmmdpb.MaterializeParentExt4Request{
		StorageKey: parentKey,
		TargetDir:  "/var/run/faas/mat/" + parentKey,
	})
	if err != nil {
		t.Fatalf("MaterializeParentExt4: %v", err)
	}
}

func TestServer_MaterializeParentExt4_MissingArgs_Mega4(t *testing.T) {
	t.Parallel()
	f := &fakeVMM{}
	cli, _ := newServer(t, f)
	_, err := cli.MaterializeParentExt4(context.Background(), &vmmdpb.MaterializeParentExt4Request{
		StorageKey: "",
		TargetDir:  "",
	})
	if err == nil {
		t.Fatal("want err for empty args")
	}
}

func TestServer_MaterializeParentExt4_NotInAllowList_Mega4(t *testing.T) {
	t.Parallel()
	f := &fakeVMM{}
	cli, _ := newServer(t, f)
	_, err := cli.MaterializeParentExt4(context.Background(), &vmmdpb.MaterializeParentExt4Request{
		StorageKey: "k-1", // not a parent-base key
		TargetDir:  "/var/run/faas/mat/k-1",
	})
	if err == nil {
		t.Fatal("want err for non-allow-listed key")
	}
}

// --- helpers -----------------------------------------------------

// errFake is a sentinel for tests that need to surface a Manager
// error through the gRPC layer.
var errFake = &api.Problem{
	Code:   "test_internal",
	Title:  "fake",
	Status: int(codes.Internal),
}
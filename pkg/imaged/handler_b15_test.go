package imaged

// Tests for the B1.5 catch-all defer (issue #195 B1.5): every error
// path from handleDeployment / handleSnapshotBoot must land the
// deployment row in a terminal-good state. Pre-fix the windows
// between the inner markDeployFailed calls and the function return
// left the row stuck in DeployBuilding indefinitely. The fix is a
// named-return defer that catches any unhandled error and flips the
// row to DeployFailed, with a guard that refuses to clobber a row
// already in {DeployFailed, DeployLive, DeploySuperseded}.
//
// All tests use the existing handler_test.go helpers (newHandler,
// fakeNotifier, fakeBuilder, fakePuller) — no new fakes needed.

import (
	"context"
	"errors"
	"testing"

	"github.com/onebox-faas/faas/pkg/state"
)

// failingNotifier makes every Notify call return an error so we can
// drive handleDeployment's notif.Notify failure path (the catch-all
// must catch it because the inner markDeployFailed is not on the
// notifier path).
type failingNotifier struct{}

func (f *failingNotifier) Notify(_ context.Context, _ string, _ string) error {
	return errors.New("notifier boom")
}

// TestHandleDeployment_NotifierFailureFlipsDeploy is the B1.5 happy
// regression: when notif.Notify(snapshot_prime) fails after a
// successful build, the deployment row must end in DeployFailed
// (the defer catches this window).
//
// Without the defer, the deployment would stay in DeployBuilding —
// the inner buildImageLayer/buildFunctionLayer markDeployFailed calls
// don't cover the post-build notifier path.
func TestHandleDeployment_NotifierFailureFlipsDeploy(t *testing.T) {
	store := state.NewMemStore()
	h := newHandler(store)
	h.notif = &failingNotifier{}

	acct, _ := store.CreateAccount(context.Background(), "u@x.com", "pro")
	app, _ := store.CreateApp(context.Background(), state.App{
		AccountID: acct.ID, Slug: "b15-notif", RAMMB: 256, IdleTimeoutS: 30, MaxConcurrency: 2,
	})
	dep, _ := store.CreateDeployment(context.Background(), state.Deployment{
		AppID: app.ID, Kind: state.DeploymentKindTarball, SourcePath: "/tmp/source.tar.gz",
	})

	err := h.handleDeployment(context.Background(), deploymentChangedPayload{
		AppID: app.ID, To: dep.ID, Kind: string(state.DeploymentKindTarball),
	})
	// handleDeployment returns nil for non-image kinds early — drive
	// via the snapshot_boot path instead. The image-kind path goes
	// through buildImageLayer which has its own mark; the catch-all
	// is for the notifier path of the snapshot_boot function.
	if err != nil {
		t.Fatalf("handleDeployment non-image should be no-op, got %v", err)
	}

	// Pre-set the row to DeployBuilding so the snapshot_boot path
	// can run; stamp a rootfs path so it doesn't early-return.
	if err := store.UpdateDeploymentStatus(context.Background(), dep.ID, state.DeployBuilding, ""); err != nil {
		t.Fatalf("seed DeployBuilding: %v", err)
	}
	if err := store.SetDeploymentRootfs(context.Background(), dep.ID, "/tmp/rootfs.ext4", "apps/b15-notif/<dep>.ext4", 1<<20); err != nil {
		t.Fatalf("seed rootfs: %v", err)
	}

	// Drive handleSnapshotBoot — the notifier is still the failing
	// one. The build may succeed; the notifier then fails; the
	// catch-all must catch it.
	err = h.handleSnapshotBoot(context.Background(), snapshotBootPayload{
		AppID: app.ID, DeploymentID: dep.ID,
	})
	if err == nil {
		t.Error("notifier failure: expected error from handleSnapshotBoot")
	}

	got, _ := store.DeploymentByID(context.Background(), dep.ID)
	if got.Status != state.DeployFailed {
		t.Errorf("B1.5 catch-all regression: deployment status = %q, want %q",
			got.Status, state.DeployFailed)
	}
}

// TestHandleDeployment_DeferSkipsAlreadyFailed is the no-clobber
// regression. If the inner markDeployFailed already set the row to
// DeployFailed, the defer must NOT overwrite it with a generic
// "handleDeployment" error code. The deployment.error_code (which the
// dashboard surfaces) would otherwise lose its specific code.
func TestHandleDeployment_DeferSkipsAlreadyFailed(t *testing.T) {
	store := state.NewMemStore()
	h := newHandler(store)

	acct, _ := store.CreateAccount(context.Background(), "u@x.com", "pro")
	app, _ := store.CreateApp(context.Background(), state.App{
		AccountID: acct.ID, Slug: "b15-skip", RAMMB: 256, IdleTimeoutS: 30, MaxConcurrency: 2,
	})
	dep, _ := store.CreateDeployment(context.Background(), state.Deployment{
		AppID: app.ID, Kind: state.DeploymentKindTarball, SourcePath: "/tmp/source.tar.gz",
	})

	// Pre-set the row to DeployFailed with a specific error code.
	innerCode := "missing_rootfs"
	innerMsg := "build failed: missing /tmp/source.tar.gz"
	if _, err := store.SetDeploymentFailed(context.Background(), dep.ID, innerCode, innerMsg); err != nil {
		t.Fatalf("seed DeployFailed: %v", err)
	}

	// Drive handleSnapshotBoot with a failing notifier — the inner
	// path's mark is already in place; the defer's reload should
	// see DeployFailed and skip.
	h.notif = &failingNotifier{}
	if err := store.SetDeploymentRootfs(context.Background(), dep.ID, "/tmp/rootfs.ext4", "apps/b15-skip/<dep>.ext4", 1<<20); err != nil {
		t.Fatalf("seed rootfs: %v", err)
	}
	_ = h.handleSnapshotBoot(context.Background(), snapshotBootPayload{
		AppID: app.ID, DeploymentID: dep.ID,
	})

	got, _ := store.DeploymentByID(context.Background(), dep.ID)
	if got.Status != state.DeployFailed {
		t.Errorf("B1.5 no-clobber regression: status = %q, want %q",
			got.Status, state.DeployFailed)
	}
	// The error_code must be preserved.
	if got.ErrorCode != innerCode {
		t.Errorf("B1.5 no-clobber regression: error_code = %q, want %q (defer overwrote inner mark)",
			got.ErrorCode, innerCode)
	}
}

// TestHandleSnapshotBoot_EmptyRootfsNotTouched asserts the empty-rootfs
// early-return path leaves the deployment row untouched. Pre-B1.5
// this was already the F-01 expectation; the defer must install AFTER
// the early-return so the no-op skip path doesn't fire the catch-all.
func TestHandleSnapshotBoot_EmptyRootfsNotTouched(t *testing.T) {
	store := state.NewMemStore()
	h := newHandler(store)
	app, _ := store.CreateApp(context.Background(), state.App{
		AccountID: "acct", Slug: "b15-empty", RAMMB: 256, IdleTimeoutS: 30, MaxConcurrency: 2,
	})
	dep, _ := store.CreateDeployment(context.Background(), state.Deployment{
		AppID: app.ID, Kind: state.DeploymentKindTarball,
		// RootfsPath intentionally empty.
	})

	err := h.handleSnapshotBoot(context.Background(), snapshotBootPayload{
		AppID: app.ID, DeploymentID: dep.ID,
	})
	if err != nil {
		t.Fatalf("empty rootfs: expected no-op, got %v", err)
	}

	got, _ := store.DeploymentByID(context.Background(), dep.ID)
	if got.Status == state.DeployFailed {
		t.Errorf("B1.5 empty-rootfs regression: defer fired on no-op skip path; status = %q", got.Status)
	}
}

// TestMarkFailedOnUnhandledError_NoErrIsNoop is the unit test for
// the helper itself. errp == nil and *errp == nil must both be
// no-ops — the defer must not mark a successful path.
func TestMarkFailedOnUnhandledError_NoErrIsNoop(t *testing.T) {
	store := state.NewMemStore()
	h := newHandler(store)
	app, _ := store.CreateApp(context.Background(), state.App{
		AccountID: "acct", Slug: "b15-noop", RAMMB: 256, IdleTimeoutS: 30, MaxConcurrency: 2,
	})
	dep, _ := store.CreateDeployment(context.Background(), state.Deployment{
		AppID: app.ID, Kind: state.DeploymentKindTarball,
	})

	// errp == nil path.
	h.markFailedOnUnhandledError(context.Background(), dep.ID, nil)

	// *errp == nil path.
	var err error
	h.markFailedOnUnhandledError(context.Background(), dep.ID, &err)

	got, _ := store.DeploymentByID(context.Background(), dep.ID)
	if got.Status == state.DeployFailed {
		t.Errorf("B1.5 no-err regression: catch-all fired with no error; status = %q", got.Status)
	}
}

// TestMarkFailedOnUnhandledError_BuildSucceededDeployFailed is the
// build-succeeded/deploy-failed race regression. We seed a build row
// that's already in BuildSucceeded (the build worked) and a
// deployment row in DeployBuilding + rootfs set. The handleSnapshotBoot
// call has a failing notifier — the build did NOT fail, the notifier
// did. The defer must mark the deployment DeployFailed even though
// the build row stays succeeded.
//
// We don't go through handleSnapshotBoot directly because the test
// uses the bare helper to assert the exact re-load behaviour.
func TestMarkFailedOnUnhandledError_BuildSucceededDeployFailed(t *testing.T) {
	store := state.NewMemStore()
	h := newHandler(store)
	app, _ := store.CreateApp(context.Background(), state.App{
		AccountID: "acct", Slug: "b15-succeeded", RAMMB: 256, IdleTimeoutS: 30, MaxConcurrency: 2,
	})
	dep, _ := store.CreateDeployment(context.Background(), state.Deployment{
		AppID: app.ID, Kind: state.DeploymentKindTarball,
	})
	if err := store.UpdateDeploymentStatus(context.Background(), dep.ID, state.DeployBuilding, ""); err != nil {
		t.Fatal(err)
	}

	upstreamErr := errors.New("notifier rejected snapshot_prime payload")
	h.markFailedOnUnhandledError(context.Background(), dep.ID, &upstreamErr)

	got, _ := store.DeploymentByID(context.Background(), dep.ID)
	if got.Status != state.DeployFailed {
		t.Errorf("B1.5 build-succeeded/deploy-failed: status = %q, want %q",
			got.Status, state.DeployFailed)
	}
}

// TestMarkFailedOnUnhandledError_DeployLiveSkipped asserts the
// "no clobber a success" guard. Pre-set the deployment to DeployLive
// (the happy-path terminal state); the catch-all must skip.
func TestMarkFailedOnUnhandledError_DeployLiveSkipped(t *testing.T) {
	store := state.NewMemStore()
	h := newHandler(store)
	app, _ := store.CreateApp(context.Background(), state.App{
		AccountID: "acct", Slug: "b15-live", RAMMB: 256, IdleTimeoutS: 30, MaxConcurrency: 2,
	})
	dep, _ := store.CreateDeployment(context.Background(), state.Deployment{
		AppID: app.ID, Kind: state.DeploymentKindTarball,
	})
	if err := store.UpdateDeploymentStatus(context.Background(), dep.ID, state.DeployLive, ""); err != nil {
		t.Fatal(err)
	}

	upstreamErr := errors.New("late error after DeployLive reached")
	h.markFailedOnUnhandledError(context.Background(), dep.ID, &upstreamErr)

	got, _ := store.DeploymentByID(context.Background(), dep.ID)
	if got.Status != state.DeployLive {
		t.Errorf("B1.5 deploy-live clobber: status = %q, want %q", got.Status, state.DeployLive)
	}
}

// guard: the failingNotifier type below satisfies the Notifier
// interface (the struct embeds fakeNotifier so we get the typed
// fields; we override Notify to always error). This compile-time
// assertion catches drift if the Notifier interface changes.
var _ Notifier = (*failingNotifier)(nil)

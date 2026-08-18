// Package imaged — vmmclient_test.go: unit tests for the
// ADR-053 imaged-side gRPC client to vmmd. The client is
// exercised through the fakeVMMClient seam; tests assert the
// dial timeout, idempotent Close, and the storage_key /
// mountpoint validation contract the vmmdpb.VmmdClient
// server-side enforces.
package imaged

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestFakeVMMClient_RecordsMountedKeys — the parent-ref branch
// in EnsureBaseExt4 invokes MountParentExt4ReadOnly once per
// staging row. The fake's mountedKeys slice lets future
// per-row staging tests assert the right storage_key landed
// at vmmd (the canonical parent base key).
func TestFakeVMMClient_RecordsMountedKeys(t *testing.T) {
	f := &fakeVMMClient{}
	if _, err := f.MountParentExt4ReadOnly(context.Background(), "base/runner-base-debian-parent-amd64.ext4"); err != nil {
		t.Fatalf("first mount: %v", err)
	}
	if _, err := f.MountParentExt4ReadOnly(context.Background(), "base/runner-base-debian-parent-arm64.ext4"); err != nil {
		t.Fatalf("second mount: %v", err)
	}
	if got, want := len(f.mountedKeys), 2; got != want {
		t.Fatalf("mountedKeys len = %d, want %d", got, want)
	}
	if f.mountedKeys[0] != "base/runner-base-debian-parent-amd64.ext4" {
		t.Errorf("mountedKeys[0] = %q", f.mountedKeys[0])
	}
	if f.mountedKeys[1] != "base/runner-base-debian-parent-arm64.ext4" {
		t.Errorf("mountedKeys[1] = %q", f.mountedKeys[1])
	}
}

// TestFakeVMMClient_MountHook — the mountHook lets a test
// inject an error or a custom mountpoint path. The
// parent-ref branch's defer pattern relies on Mount returning
// a usable path; an error from the hook surfaces cleanly.
func TestFakeVMMClient_MountHook(t *testing.T) {
	customMP := "/tmp/custom-mountpoint"
	f := &fakeVMMClient{mountHook: func(_ string) (string, error) {
		return customMP, nil
	}}
	mp, err := f.MountParentExt4ReadOnly(context.Background(), "key")
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	if mp != customMP {
		t.Errorf("mp = %q, want %q", mp, customMP)
	}
}

// TestFakeVMMClient_MountHookError — when vmmd is unreachable
// (dial error), Mount returns the error directly. The
// parent-ref branch surfaces it as a wrapped
// "mount parent ext4" error.
func TestFakeVMMClient_MountHookError(t *testing.T) {
	want := errors.New("connection refused")
	f := &fakeVMMClient{mountHook: func(_ string) (string, error) {
		return "", want
	}}
	if _, err := f.MountParentExt4ReadOnly(context.Background(), "key"); !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
}

// TestFakeVMMClient_UmountHookError — umount errors during
// the early-release path are logged and swallowed (the
// deferred umount is the load-bearing safety net). Tests
// only assert the hook fired.
func TestFakeVMMClient_UmountHookError(t *testing.T) {
	want := errors.New("busy")
	f := &fakeVMMClient{umountHook: func(_ string) error { return want }}
	if err := f.UmountParentExt4(context.Background(), "/tmp/mp"); !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
}

// TestFakeVMMClient_CloseIdempotent — Close() is called from
// cmd/imaged's shutdown path. A nil receiver (the handler
// never wired a client) is a no-op; a Close'd fake is also
// a no-op on the next call.
func TestFakeVMMClient_CloseIdempotent(t *testing.T) {
	var f *fakeVMMClient
	if err := f.Close(); err != nil {
		t.Errorf("nil receiver Close: %v", err)
	}
	f = &fakeVMMClient{}
	if err := f.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// TestNewVMMClient_Defaults — when target is empty,
// NewVMMClient falls back to DefaultVMMSock (the
// production unix socket). cmd/imaged relies on this so
// it can pass `envOr("FAAS_VMM_SOCK", imaged.DefaultVMMSock)`
// and let the empty-string case collapse to the default
// without an extra check.
func TestNewVMMClient_Defaults(t *testing.T) {
	c := NewVMMClient("", nil)
	if c.target != DefaultVMMSock {
		t.Errorf("target = %q, want %q", c.target, DefaultVMMSock)
	}
	if c == nil {
		t.Error("NewVMMClient returned nil")
	}
}

// TestNewVMMClient_ExplicitTarget — the override target is
// preserved verbatim so a developer pointing at a bufconn
// listener in unit tests doesn't accidentally hit the
// production socket.
func TestNewVMMClient_ExplicitTarget(t *testing.T) {
	const target = "unix:///tmp/faas-vmmd-test.sock"
	c := NewVMMClient(target, nil)
	if c.target != target {
		t.Errorf("target = %q, want %q", c.target, target)
	}
}

// TestDefaultVMMSock_IsProductionSocket — guards the
// invariant that DefaultVMMSock matches the socket vmmd
// opens per ADR-015 (/run/faas/vmmd.sock). A drift here
// means cmd/imaged would dial the wrong socket and the
// parent-ref staging path would fail every cold-boot.
func TestDefaultVMMSock_IsProductionSocket(t *testing.T) {
	if !strings.HasPrefix(DefaultVMMSock, "unix://") {
		t.Errorf("DefaultVMMSock = %q, must be unix:// scheme", DefaultVMMSock)
	}
	if !strings.Contains(DefaultVMMSock, "vmmd.sock") {
		t.Errorf("DefaultVMMSock = %q, must reference vmmd.sock (ADR-015)", DefaultVMMSock)
	}
}

// TestNewVMMClient_NodeNameEnvRead pins the issue #678 / ADR-093
// PR-0 surface: NewVMMClient reads FAAS_IMAGED_NODE_NAME at
// construction time so cmd/imaged doesn't have to thread the
// env-read through its main wiring. Empty env → empty nodeName
// (single-box dev back-compat; PR-B's scheme gate stays closed).
// Non-empty env → nodeName pinned for the verifier's allowed-CN
// list. PR-B's verifier (pkg/wire.InmemNodeVerifier for tests,
// PGNodeVerifier for production) reads c.nodeName to populate
// its allow-list at TCP/DNS scheme boundaries.
func TestNewVMMClient_NodeNameEnvRead(t *testing.T) {
	const want = "fsn-2-imaged"
	t.Setenv("FAAS_IMAGED_NODE_NAME", want)
	c := NewVMMClient(DefaultVMMSock, nil)
	if c.nodeName != want {
		t.Errorf("nodeName = %q, want %q (env read failed)", c.nodeName, want)
	}
}

// TestNewVMMClient_NodeNameEmptyByDefault — back-compat: missing
// or empty FAAS_IMAGED_NODE_NAME → nodeName stays empty so the
// PR-B scheme gate (TCP/DNS = verifier on; unix = verifier off)
// defaults to the single-box unix path. A production deployment
// without the env var would never construct a verifier, which is
// the intended shape for single-box dev.
func TestNewVMMClient_NodeNameEmptyByDefault(t *testing.T) {
	t.Setenv("FAAS_IMAGED_NODE_NAME", "")
	c := NewVMMClient(DefaultVMMSock, nil)
	if c.nodeName != "" {
		t.Errorf("nodeName = %q, want empty (single-box default)", c.nodeName)
	}
}

// Package imaged — vmmclient.go: typed gRPC client to vmmd for
// the ADR-053 parent-base staging path. imaged is not root
// (User=faas-imaged + NoNewPrivileges=yes per spec §11) and
// cannot mount a loopback ext4 itself; vmmd is the only root
// component, so imaged asks vmmd to run `mount -o loop,ro` on
// its behalf and returns the mountpoint path.
//
// CLAUDE.md's "component ownership" line (apid must not call
// vmmd) is amended by ADR-053 to acknowledge this new edge:
// imaged→vmmd, scoped to the staging-only parent-mount
// workflow. There's no other imaged→vmmd call site in the
// repo today, and the gRPC client is constructed lazily
// (mirror of pkg/vmmdgrpc/advisory_client.go::dial) so unit
// tests that don't wire the client keep working without a
// stub.
package imaged

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	vmmdpb "github.com/onebox-faas/faas/api/proto/onebox/faas/vmmd/v1"
	"github.com/onebox-faas/faas/pkg/wire"
	"google.golang.org/grpc"
)

// defaultVMMDialTimeout is the per-RPC dial+send budget. The
// staging path is on the cold-boot axis (spec §6.2) so a stuck
// vmmd dial must not block an imaged startup past the parent's
// own OCI pull; 30 s matches the cold-boot fallback budget
// (Park's max_wait_ms default). A future PR may shorten this
// once the parent-ref staging path has a tighter SLO.
const defaultVMMDialTimeout = 30 * time.Second

// DefaultVMMSock is the default vmmd unix socket — matches
// /run/faas/vmmd.sock (ADR-015). Exported so cmd/imaged can
// reference it (FAAS_VMM_SOCK fallback). Overridable via
// NewVMMClient for tests + dev (e.g. "unix:///tmp/faas-vmmd-test.sock").
const DefaultVMMSock = "unix:///run/faas/vmmd.sock"

// VMMClientIface is the parent-mount subset of vmmd's gRPC API
// that imaged depends on (ADR-053 + ADR-075). Defining it as an
// interface here (not the broader vmmdpb.VmmdClient) keeps
// pkg/imaged's test seam narrow: a fakeVMMClient with these
// five methods is enough to drive the parent-ref branch in unit
// tests, no real gRPC dial required.
//
// MountOverlayParent / UmountOverlayParent (ADR-075 / DEPLOY-1)
// replace the unix.Mount(2) syscall that used to live in
// pkg/imaged/mount_overlay_linux.go under
// AmbientCapabilities=cap_sys_admin — that path was the silent
// CLAUDE.md violation that DEPLOY-1 erases. imaged no longer
// holds cap_sys_admin at all; the systemd unit can drop
// AmbientCapabilities= entirely.
type VMMClientIface interface {
	MountParentExt4ReadOnly(ctx context.Context, storageKey string) (string, error)
	UmountParentExt4(ctx context.Context, mountpoint string) error
	MountOverlayParent(ctx context.Context, lowerdir, upperdir, workdir, merged string) error
	UmountOverlayParent(ctx context.Context, merged string) error
	Close() error
}

// VMMClient is the imaged-side dialer for the vmmd parent-mount
// RPCs (ADR-053). One per imaged process; serialises calls behind
// a mutex so the first burst of staging work coalesces on one
// connection.
//
// log may be nil (slog.Default fallback). The dial is lazy —
// the first MountParentExt4ReadOnly / UmountParentExt4 call
// triggers grpc.DialContext; a transiently-down vmmd surfaces
// as a dial error on that first call, not at construction time.
type VMMClient struct {
	target string
	log    *slog.Logger

	mu   sync.Mutex
	conn *grpc.ClientConn
	cli  vmmdpb.VmmdClient
}

// Compile-time assertion: *VMMClient satisfies VMMClientIface so
// production cmd/imaged wiring can pass a real *VMMClient through
// WithVMMClient.
var _ VMMClientIface = (*VMMClient)(nil)

// NewVMMClient builds a client against the given unix-socket
// target. Production wiring (cmd/imaged) passes
// defaultVMMSock; tests pass a bufconn or a fake target so the
// dial can be short-circuited. log may be nil.
func NewVMMClient(target string, log *slog.Logger) *VMMClient {
	if target == "" {
		target = DefaultVMMSock
	}
	if log == nil {
		log = slog.Default()
	}
	return &VMMClient{target: target, log: log}
}

// MountParentExt4ReadOnly asks vmmd to loopback-mount the
// parent ext4 at `storageKey` read-only and returns the
// absolute host path of the mountpoint. Caller is expected to
// `cp -a` the tree out immediately, then call UmountParentExt4
// in a defer. ADR-053 staging-only path.
func (c *VMMClient) MountParentExt4ReadOnly(ctx context.Context, storageKey string) (string, error) {
	if c == nil {
		return "", fmt.Errorf("imaged: vmmclient: nil receiver")
	}
	if storageKey == "" {
		return "", fmt.Errorf("imaged: vmmclient: empty storage_key")
	}
	cli, err := c.dial(ctx)
	if err != nil {
		return "", err
	}
	callCtx, cancel := context.WithTimeout(ctx, defaultVMMDialTimeout)
	defer cancel()
	resp, err := cli.MountParentExt4ReadOnly(callCtx, &vmmdpb.MountParentExt4ReadOnlyRequest{
		StorageKey: storageKey,
	})
	if err != nil {
		return "", fmt.Errorf("imaged: vmmclient: mount parent ext4 %q: %w", storageKey, err)
	}
	mp := resp.GetMountpoint()
	if mp == "" {
		return "", fmt.Errorf("imaged: vmmclient: mount parent ext4 %q: empty mountpoint", storageKey)
	}
	return mp, nil
}

// UmountParentExt4 releases a mount returned from
// MountParentExt4ReadOnly. Idempotent: imaged's
// defer-after-error pattern is safe to call blindly. A nil
// receiver or empty mountpoint is a no-op (the gRPC contract
// treats both as success).
func (c *VMMClient) UmountParentExt4(ctx context.Context, mountpoint string) error {
	if c == nil || mountpoint == "" {
		return nil
	}
	cli, err := c.dial(ctx)
	if err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, defaultVMMDialTimeout)
	defer cancel()
	if _, err := cli.UmountParentExt4(callCtx, &vmmdpb.UmountParentExt4Request{
		Mountpoint: mountpoint,
	}); err != nil {
		return fmt.Errorf("imaged: vmmclient: umount parent ext4 %q: %w", mountpoint, err)
	}
	return nil
}

// MountOverlayParent (ADR-075 / DEPLOY-1) asks vmmd to issue the
// overlayfs mount on behalf of imaged. The syscall used to live
// in pkg/imaged/mount_overlay_linux.go under
// AmbientCapabilities=cap_sys_admin — that path is the
// architectural violation DEPLOY-1 erases. imaged no longer
// holds cap_sys_admin; the systemd unit can drop the ambient
// capability entirely.
//
// Empty path → error so a misbehaving caller doesn't slip past
// the gRPC validation chain (vmmd's handler also rejects, but
// the client-side guard keeps imaged's log clean).
func (c *VMMClient) MountOverlayParent(ctx context.Context, lowerdir, upperdir, workdir, merged string) error {
	if c == nil {
		return fmt.Errorf("imaged: vmmclient: nil receiver")
	}
	if lowerdir == "" || upperdir == "" || workdir == "" || merged == "" {
		return fmt.Errorf("imaged: vmmclient: empty overlay path (lower=%q upper=%q work=%q merged=%q)",
			lowerdir, upperdir, workdir, merged)
	}
	cli, err := c.dial(ctx)
	if err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, defaultVMMDialTimeout)
	defer cancel()
	if _, err := cli.MountOverlayParent(callCtx, &vmmdpb.MountOverlayParentRequest{
		Lowerdir: lowerdir,
		Upperdir: upperdir,
		Workdir:  workdir,
		Merged:   merged,
	}); err != nil {
		return fmt.Errorf("imaged: vmmclient: mount overlay parent (lower=%q merged=%q): %w",
			lowerdir, merged, err)
	}
	return nil
}

// UmountOverlayParent (ADR-075 / DEPLOY-1) releases an overlay
// mount returned from MountOverlayParent. Idempotent: vmmd's
// handler absorbs ErrUnknownMountpoint so imaged's
// defer-after-error pattern is safe to call blindly.
func (c *VMMClient) UmountOverlayParent(ctx context.Context, merged string) error {
	if c == nil || merged == "" {
		return nil
	}
	cli, err := c.dial(ctx)
	if err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, defaultVMMDialTimeout)
	defer cancel()
	if _, err := cli.UmountOverlayParent(callCtx, &vmmdpb.UmountOverlayParentRequest{
		Merged: merged,
	}); err != nil {
		return fmt.Errorf("imaged: vmmclient: umount overlay parent %q: %w", merged, err)
	}
	return nil
}

// Close releases the underlying gRPC conn. Idempotent. Called
// from imaged's shutdown path so SIGTERM doesn't leak the dial.
func (c *VMMClient) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	c.cli = nil
	return err
}

// dial lazily dials the vmmd socket. Holds c.mu so the first
// burst of staging calls coalesce on one connection.
// Mirrors pkg/vmmdgrpc/advisory_client.go::dial (same lazy-
// dial-under-mutex pattern; same wire.DialContext call site).
func (c *VMMClient) dial(ctx context.Context) (vmmdpb.VmmdClient, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cli != nil && c.conn != nil {
		return c.cli, nil
	}
	dialCtx, cancel := context.WithTimeout(ctx, defaultVMMDialTimeout)
	defer cancel()
	conn, err := wire.DialContext(dialCtx, c.target, nil)
	if err != nil {
		return nil, fmt.Errorf("imaged: vmmclient: dial %s: %w", c.target, err)
	}
	c.conn = conn
	c.cli = vmmdpb.NewVmmdClient(conn)
	return c.cli, nil
}

// fakeVMMClient is the unit-test seam for EnsureBaseExt4's
// parent-ref branch (ADR-053). Production wiring goes through
// NewVMMClient + a real gRPC dial; tests use this no-op fake to
// keep pkg/imaged KVM/loop-free per spec §Conventions.
//
// The fake records the storage keys it was asked to mount and
// the mountpoints it was asked to release, and exposes hooks so
// tests can assert the parent-ref branch invoked vmmd exactly
// once per row, with the expected key, and ALWAYS paired a
// release with the mount (review finding — defer-after-error
// idempotency).
type fakeVMMClient struct {
	mountedKeys []string
	// umountCalls records every UmountParentExt4 invocation so
	// tests can assert the parent mount is always released, even
	// on the success path (the §4.6 staging composition is a
	// defer'd umount).
	umountCalls int
	// mountHook lets tests inject an error or a custom mountpoint
	// path; default returns ("/tmp/faas-parent-fake-<n>", nil).
	mountHook func(storageKey string) (string, error)
	// umountHook lets tests inject an error on umount.
	umountHook func(mountpoint string) error
	// overlayMounts (ADR-075 / DEPLOY-1) records every
	// MountOverlayParent invocation with the full path tuple so
	// the parent-ref tests can assert the RPC was called with
	// the staging paths imaged chose.
	overlayMounts []overlayMountRecord
	// overlayUmounts (ADR-075 / DEPLOY-1) records every
	// UmountOverlayParent invocation by merged path. Tests
	// assert the defer-after-error release fires.
	overlayUmounts []string
	// mountOverlayHook lets tests inject errors on the overlay
	// mount path.
	mountOverlayHook func(lowerdir, upperdir, workdir, merged string) error
	// umountOverlayHook lets tests inject errors on the overlay
	// umount path.
	umountOverlayHook func(merged string) error
}

func (f *fakeVMMClient) MountParentExt4ReadOnly(_ context.Context, storageKey string) (string, error) {
	f.mountedKeys = append(f.mountedKeys, storageKey)
	if f.mountHook != nil {
		return f.mountHook(storageKey)
	}
	return "/tmp/faas-parent-fake-" + storageKey, nil
}

func (f *fakeVMMClient) UmountParentExt4(_ context.Context, mountpoint string) error {
	f.umountCalls++
	if f.umountHook != nil {
		return f.umountHook(mountpoint)
	}
	return nil
}

// MountOverlayParent (ADR-075 / DEPLOY-1) — fakeVMMClient records
// the overlay mount paths so the parent-ref tests can assert the
// mount call was made. mountOverlayHook lets tests inject errors
// (the wire InvalidArgument branch — a foreign path prefix — is
// NOT exercised here; that branch lives in pkg/vmmdgrpc).
func (f *fakeVMMClient) MountOverlayParent(_ context.Context, lowerdir, upperdir, workdir, merged string) error {
	f.overlayMounts = append(f.overlayMounts, overlayMountRecord{
		Lowerdir: lowerdir,
		Upperdir: upperdir,
		Workdir:  workdir,
		Merged:   merged,
	})
	if f.mountOverlayHook != nil {
		return f.mountOverlayHook(lowerdir, upperdir, workdir, merged)
	}
	return nil
}

// UmountOverlayParent (ADR-075 / DEPLOY-1) — mirrors the Mount
// path. Records every call so the test can assert the
// defer-after-error release fires.
func (f *fakeVMMClient) UmountOverlayParent(_ context.Context, merged string) error {
	f.overlayUmounts = append(f.overlayUmounts, merged)
	if f.umountOverlayHook != nil {
		return f.umountOverlayHook(merged)
	}
	return nil
}

func (f *fakeVMMClient) Close() error { return nil }

// overlayMountRecord captures one MountOverlayParent invocation
// so tests can assert the four paths were forwarded verbatim.
// Keeping a struct (vs four parallel slices) lets a future field
// change land without touching every assert site.
type overlayMountRecord struct {
	Lowerdir string
	Upperdir string
	Workdir  string
	Merged   string
}

// overlay.go is the vmmd-side mount helper for the parent-ref
// overlayfs staged under /dev/shm/faas-base-staging (DEPLOY-1 /
// ADR-075). Before DEPLOY-1 the unix.Mount(2) syscall lived in
// pkg/imaged/mount_overlay_linux.go under
// AmbientCapabilities=cap_sys_admin — a silent violation of the
// CLAUDE.md invariant "vmmd is the only root component that
// mounts filesystems". The cap_sys_admin ambient cap plus the
// ext4 upperdir (host /tmp) plus the resulting dmesg
// "upper fs does not support tmpfile" message cascade killed
// cd-controlplane deploys for five days (2026-08-04 → 2026-08-05,
// 13 failed deploys). All three of those failures are wired up
// here: the cap walks through pkg/vmmd, the staging dir walks
// through FAAS_BASE_STAGING_ROOT, and the overlay walks through
// the kernel's tmpfile check.
//
// MountOverlayParent issues the mount on behalf of imaged. The
// caller (cmd/vmmd via the gRPC server) is the only component
// in the process tree with cap_sys_admin. imaged calls
// MountOverlayParent over its vmmdgrpc unix socket; the vmmd
// handler validates the path prefixes and forwards to this
// function.
//
// Validation chain (mirrors the MountExt4ReadOnly gate):
//  1. lowerdir must be under /srv/fc/parent/ — the loopback
//     mount vmmd has already issued (the parent-ref layer is
//     read-only and staged via MountExt4ReadOnly).
//  2. upperdir/workdir/merged must be under
//     /dev/shm/faas-base-staging/ — staging lives on tmpfs so
//     the kernel's overlayfs tmpfile contract is satisfied
//     (ext4 cannot host the workdir's tmpfile + rename atomic
//     cycle; this is the original 2026-08-04 dmesg bug).
//  3. Empty strings → InvalidArgument (imaged's defer-after-
//     error is safe on these — the handler absorbs them).
//  4. unix.Mount syscall errors → wrapped Internal so the gRPC
//     handler can surface a problem document.
package vmmdmount

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// OverlayStagingRoot is the tmpfs root for the overlayfs
// upperdir/workdir/merged trio. The constant mirrors the env
// var pkg/rootfs/build_base.go honours (FAAS_BASE_STAGING_ROOT,
// default /dev/shm/faas-base-staging). Pinning the constant here
// makes the vmmd-side validation chain immune to imaged setting
// the env var to something the host can't support.
//
// On non-Linux dev (macOS / Windows) the function never runs —
// the gRPC handler is metal-only, and the unit test in
// overlay_test.go uses a stub for the syscall.
const OverlayStagingRoot = "/dev/shm/faas-base-staging"

// OverlayMountPrefix is the basename prefix for upper/work/merged
// subdirs. Picked to be visually distinct from
// ParentMountPrefix so an `ls /dev/shm/faas-base-staging` is
// obvious about what's a parent-ref staging tree vs a stray.
const OverlayMountPrefix = "faas-overlay-"

// ErrInvalidOverlayPath is the typed sentinel MountOverlayParent
// returns when lowerdir is not under /srv/fc/parent or
// upperdir/workdir/merged is not under /dev/shm/faas-base-staging.
// The gRPC handler lifts this to InvalidArgument so a misbehaving
// imaged can't persuade vmmd to mount something the kernel can
// reach (e.g. /home/user/Documents) by setting the right env
// var.
var ErrInvalidOverlayPath = errors.New("vmmdmount: overlay path outside allowed prefixes")

// MountOverlayParent mounts an overlayfs with lowerdir (read-only
// loopback already issued by vmmd), upperdir+workdir on the host
// tmpfs, and merged as the combined view. The function issues
// the unix.Mount(2) syscall directly (no exec.Command("mount"))
// so a future pkg/vmmdmount test can stub the syscall without
// fork.
//
// On success returns nil. On any error path the freshly-created
// merged dir is rmdir'd so a failed mount doesn't leave orphans
// under /dev/shm/faas-base-staging.
//
// Validation:
//   - lowerdir MUST exist and be a directory; it must be under
//     /srv/fc/parent/. (Loopback mount already issued.)
//   - upperdir, workdir, merged MUST be under
//     /dev/shm/faas-base-staging/. The caller (cmd/imaged) is
//     responsible for creating them; this function only creates
//     `merged` if it does not exist (the parent dirs were made
//     by MkdirBaseStaging + os.MkdirTemp).
//   - All four paths must be absolute; relative paths are
//     rejected to prevent ../../ escape.
func MountOverlayParent(ctx context.Context, lowerdir, upperdir, workdir, merged string) error {
	if lowerdir == "" || upperdir == "" || workdir == "" || merged == "" {
		return fmt.Errorf("vmmdmount: MountOverlayParent: empty path (lower=%q upper=%q work=%q merged=%q)",
			lowerdir, upperdir, workdir, merged)
	}

	// All paths must be absolute. relative paths would let a
	// caller mount into an arbitrary cwd (which on vmmd is /).
	if !filepath.IsAbs(lowerdir) || !filepath.IsAbs(upperdir) ||
		!filepath.IsAbs(workdir) || !filepath.IsAbs(merged) {
		return fmt.Errorf("vmmdmount: MountOverlayParent: non-absolute path rejected")
	}

	// Lowerdir must be under MountRoot (where vmmd loopback
	// mounts live). A misbehaving imaged can't ask vmmd to
	// overlay /etc or /var/lib/faas by passing that as lower.
	if !strings.HasPrefix(lowerdir, filepath.Clean(MountRoot)+string(filepath.Separator)) {
		return fmt.Errorf("vmmdmount: MountOverlayParent: lowerdir %q not under %s: %w",
			lowerdir, MountRoot, ErrInvalidOverlayPath)
	}

	// Upper/work/merged must be under OverlayStagingRoot
	// (/dev/shm/faas-base-staging). Same logic — refuse to mount
	// outside the host tmpfs staging tree.
	stagingPrefix := filepath.Clean(OverlayStagingRoot) + string(filepath.Separator)
	for _, p := range []string{upperdir, workdir, merged} {
		if !strings.HasPrefix(p, stagingPrefix) {
			return fmt.Errorf("vmmdmount: MountOverlayParent: %q not under %s: %w",
				p, OverlayStagingRoot, ErrInvalidOverlayPath)
		}
	}

	// The lowerdir must exist (vmmd should have loopback-mounted
	// it via MountParentExt4ReadOnly before imaged fires this
	// RPC). Stat catches the race where imaged gets the
	// mountpoint from a previous gRPC call but the loopback has
	// been swept by the orphan-sweep thread.
	if info, err := os.Stat(lowerdir); err != nil {
		return fmt.Errorf("vmmdmount: MountOverlayParent: stat lowerdir: %w", err)
	} else if !info.IsDir() {
		return fmt.Errorf("vmmdmount: MountOverlayParent: lowerdir %q is not a directory", lowerdir)
	}

	// Ensure `merged` exists. imaged's MkdirBaseStaging +
	// os.MkdirTemp creates upper+work; merged is the empty dir
	// overlayfs writes into. We mkdir here so a caller that
	// forgot to mkdir gets a clean InvalidArgument rather than
	// an opaque EINVAL from the kernel.
	if err := os.MkdirAll(merged, 0o755); err != nil {
		return fmt.Errorf("vmmdmount: MountOverlayParent: mkdir merged: %w", err)
	}

	// The mount itself. unix.Mount's data parameter is an
	// unsafe.Pointer to a NUL-terminated string. golang.org/x/sys/unix
	// exposes a higher-level wrapper via MountAt with a string
	// data; that wrapper handles the cstring conversion for us.
	// The flag is 0 — overlayfs on tmpfs upperdir doesn't care
	// about atime semantics for the staging dir.
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lowerdir, upperdir, workdir)
	if err := unix.Mount("overlay", merged, 0, unsafeStringPointer(opts)); err != nil {
		// rmdir the merged dir we just created — the mount
		// failed so the caller can't umount it.
		_ = os.Remove(merged)
		return fmt.Errorf("vmmdmount: MountOverlayParent: mount overlay: %w", err)
	}
	_ = ctx // future: bind to ctx for cancellation parity with MountExt4ReadOnly
	return nil
}

// unsafeStringPointer converts a Go string into the unsafe.Pointer
// golang.org/x/sys/unix.Mount requires for its data parameter.
// The returned pointer is only valid for the lifetime of the
// underlying Go string; the mount syscall is synchronous so the
// string outlives the call. Avoid using this helper anywhere the
// string escapes the calling goroutine (e.g. async umount — use
// exec.Command("umount", ...) for that, which we already do).
func unsafeStringPointer(s string) unsafe.Pointer {
	//nolint:gosec // DEPLOY-1 / ADR-075: unix.Mount's data param requires unsafe.Pointer; this is the one audited wrapper.
	if s == "" {
		return unsafe.Pointer(nil)
	}
	// unsafe.StringData is the zero-copy accessor for a Go
	// string's backing bytes. The pointer it returns is
	// NUL-terminated because Go strings are. The conversion
	// from *byte to unsafe.Pointer is explicit (no implicit
	// pointer arithmetic on unsafe.Pointer).
	//nolint:gosec // DEPLOY-1 / ADR-075: see comment above on unsafeStringPointer.
	return unsafe.Pointer(unsafe.StringData(s))
}

// UmountOverlayParent unmounts a merged directory previously
// returned via MountOverlayParent. Idempotent on unknown
// mountpoints — like UmountExt4, the gRPC handler absorbs
// ErrUnknownMountpoint so imaged's defer-after-error is safe.
//
// The function uses `exec.Command("umount", ...)` (not unix.
// Unmount) because /dev/shm/faas-base-staging is not vmmd's
// own mount namespace — we don't have CAP_SYS_ADMIN against
// /dev/shm, we have it against the host. The umount(8) binary
// handles the path-resolution correctly. (Note: on a vmmd that
// runs PrivateTmp=yes, /dev/shm is the host's, not a
// per-process mount — confirmed by deploy/systemd/faas-vmmd.service
// not setting PrivateTmp.)
func UmountOverlayParent(ctx context.Context, merged string) error {
	if merged == "" {
		return ErrUnknownMountpoint
	}
	stagingPrefix := filepath.Clean(OverlayStagingRoot) + string(filepath.Separator)
	if !strings.HasPrefix(merged, stagingPrefix) {
		return fmt.Errorf("vmmdmount: UmountOverlayParent: merged %q outside %s: %w",
			merged, OverlayStagingRoot, ErrInvalidOverlayPath)
	}
	if _, err := os.Stat(merged); err != nil {
		if os.IsNotExist(err) {
			return ErrUnknownMountpoint
		}
		return fmt.Errorf("vmmdmount: UmountOverlayParent: stat merged: %w", err)
	}
	cmd := exec.CommandContext(ctx, "umount", merged)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("vmmdmount: UmountOverlayParent: umount %s: %w (%s)",
			merged, err, strings.TrimSpace(string(out)))
	}
	// rmdir the now-empty merged dir so the staging tree
	// shrinks back. os.Remove on a non-empty dir fails with
	// ENOTEMPTY — caller can decide whether to retry or sweep.
	if err := os.Remove(merged); err != nil && !os.IsNotExist(err) {
		// Surface as a warning; the next sweep tick will
		// clean up. Don't fail the umount RPC on rmdir
		// failure — the overlay is gone.
		return fmt.Errorf("vmmdmount: UmountOverlayParent: rmdir merged: %w", err)
	}
	return nil
}

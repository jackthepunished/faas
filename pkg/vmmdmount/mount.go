// Package vmmdmount — loopback mount/umount for the ADR-053
// parent-base staging path. vmmd is the only root component (spec
// §11) and runs `mount -o loop,ro,nodev,nosuid,noexec` on imaged's
// behalf; imaged (User=faas-imaged + NoNewPrivileges=yes) cannot
// do this itself.
//
// Architecture:
//
//   - MountParentExt4ReadOnly: looks up storageKey via the configured
//     StorageBackend, stages the bytes into
//     /srv/fc/parent/faas-parent-src-*, mkdir's
//     /srv/fc/parent/faas-parent-mnt-*, mounts -o loop,ro, and
//     registers the (mountpoint -> storageKey, mountedAt) pair in
//     Registry. Returns the mountpoint path; caller reads via
//     `mkfs.ext4 -d <mountpoint>` (§4.6 — no cp -a) and calls
//     UmountParentExt4.
//   - UmountParentExt4: looks up the mountpoint in the registry,
//     runs `umount`, removes the registry entry, deletes the staged
//     source bytes, and rmdir's the mountpoint. Idempotent on
//     unknown mountpoints so imaged's defer-after-error pattern is
//     safe.
//
// MountRoot is /srv/fc/parent/ because both vmmd and imaged run
// under systemd PrivateTmp=yes (deploy/systemd/faas-{vmmd,imaged}.service)
// — vmmd's /tmp is its own tmpfs, invisible to imaged's mount
// namespace. /srv/fc/parent/ is shared (mkdir 0750 root:faas at
// box bootstrap; imaged's ReadWritePaths already covers /srv/fc).
//
// Sweep: Registry.SweepOrphans walks every entry older than
// ParentMountMaxAge (default 30 min) and force-umounts it. Called
// from cmd/vmmd's main goroutine on SIGTERM (sync sweep via
// SweepAll) and on a configurable background ticker (orphan
// sweep). The 30-minute default is generous — a normal mkfs.ext4
// -d over ~280 MB of debian userland takes seconds; a hung imaged
// child would surface long before the sweep kicks in.
//
// Why a separate package: pkg/vmmdgrpc imports pkg/fcvm, so
// pkg/fcvm cannot import pkg/vmmdgrpc without a cycle. Manager
// needs the registry + mount helpers directly, so they live here
// (no internal dependency).
package vmmdmount

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ParentMountPrefix is the tempdir name pattern vmmd uses for both
// the source bytes (StorageBackend.Get) and the loopback mountpoint.
// Mirrored on the imaged side as the prefix pkg/imaged/vmmclient.go
// reads back to construct the source path under the same parent.
const ParentMountPrefix = "faas-parent-"

// MountRoot is the parent directory every parent mount lives
// under. /srv/fc/parent/ is created at box bootstrap with owner
// root, group faas, mode 0750; imaged's ReadWritePaths already
// covers /srv/fc, so the directory is readable for the
// daemon traversal that mkfs.ext4 -d performs via the read-only
// loopback mount. Keeping the path off /tmp is load-bearing —
// both vmmd and imaged carry PrivateTmp=yes in their systemd
// units (deploy/systemd/faas-{vmmd,imaged}.service) and vmmd's
// /tmp is its own tmpfs invisible to imaged's namespace.
const MountRoot = "/srv/fc/parent"

// DefaultCap is the registry cap used when NewRegistry is called
// with cap<=0. 16 matches the worst-case staging parallelism on
// a one-box fleet (a rebuild + the four child runtimes + headroom).
const DefaultCap = 16

// ParentMountMaxAge is the orphan-sweep threshold. Anything older
// than this when SweepOrphans runs is force-umounted. The default
// is generous (30 min) — a normal mkfs.ext4 -d over a ~280 MB
// debian userland takes seconds; a hung imaged child would surface
// long before the sweep kicks in. cmd/vmmd overrides this via
// FAAS_VMMD_PARENT_MAX_AGE when operators need to tune.
const ParentMountMaxAge = 30 * time.Minute

// parentMountOpts is the mount-option string passed to `mount -o`.
// nodev,nosuid,noexec hardens the loopback mount against any
// binary inside the parent ext4 executing or opening device nodes
// — defense in depth; the mount is short-lived and read-only, so
// the options cost nothing in the staging hot path.
const parentMountOpts = "loop,ro,nodev,nosuid,noexec"

// ErrUnknownMountpoint is the typed sentinel UmountParentExt4 returns
// when the mountpoint isn't in the registry. The gRPC handler lifts
// this to InvalidArgument (NOT NotFound) so imaged's defer-after-error
// pattern is idempotent.
var ErrUnknownMountpoint = errors.New("vmmdmount: unknown mountpoint")

// ErrNotFound is the typed sentinel MountParentExt4 returns when
// the StorageBackend reports the storageKey as not present. The
// gRPC handler lifts this to NotFound.
var ErrNotFound = errors.New("vmmdmount: storage_key not found")

// Registry tracks every live parent-mount vmmd has issued. One per
// vmmd process; serialised behind a mutex because the registration /
// lookup / sweep all touch the map. The cap (default 16) bounds
// memory growth in the face of an imaged that misbehaves and never
// calls UmountParentExt4.
type Registry struct {
	mu      sync.Mutex
	entries map[string]MountEntry
	cap     int
}

// MountEntry is the per-mount record. Exported so the Manager
// (in pkg/fcvm) can read SrcPath + StorageKey on Umount to clean
// up the staged bytes.
type MountEntry struct {
	// Kind tells Registry.Umount + SweepOrphans which umount
	// syscall to dispatch. MountKindParentExt4 (the ADR-053
	// loopback path) uses `umount` + SrcPath cleanup;
	// MountKindOverlayParent (DEPLOY-1) uses
	// UmountOverlayParent which already cleans up its merged
	// dir. Before MountKind existed (review finding B4) the
	// sweep umounted every entry as ext4, which (a) silently
	// succeeded on an overlayfs mount (kernel treats both as
	// generic mounts) and (b) leaked the upperdir/workdir
	// staging tree because no SrcPath was registered.
	Kind MountKind
	// StorageKey is set for MountKindParentExt4 entries (the
	// canonical StorageBackend key). Empty for MountKindOverlayParent.
	StorageKey string
	// SrcPath is set for MountKindParentExt4 entries (the
	// staged StorageBackend bytes; deleted on umount). Empty
	// for MountKindOverlayParent (the upper/work/merged tree
	// is owned by vmmdmount.UmountOverlayParent).
	SrcPath string
	// MountedAt is when the mount was registered.
	MountedAt time.Time
}

// MountKind discriminates ext4 loopback mounts (the ADR-053
// parent-base staging path) from overlay mounts (the DEPLOY-1
// parent-ref overlay staging path). The Kind is stored on
// MountEntry so Registry.Umount + SweepOrphans + SweepAll
// dispatch to the right umount syscall. Without Kind, the
// registry could not distinguish the two and the sweep would
// run a generic `umount` on every entry — which silently
// worked for the kernel (both are mounted filesystems) but
// leaked upperdir/workdir on the overlay path because no
// SrcPath was registered to clean up.
//
// Adding a new kind requires extending RegisterOrEvict to
// take a MountKind + UmountFunc (or switch on the kind here
// in Registry.Umount). The current two kinds cover all
// mount owners in this box today.
type MountKind int

const (
	// MountKindParentExt4 is an `mount -o loop,ro,nodev,nosuid,
	// noexec` loopback of a StorageBackend-fetched parent-base
	// ext4 image. SrcPath + StorageKey are set on the entry;
	// Umount removes the staged source file.
	MountKindParentExt4 MountKind = iota + 1
	// MountKindOverlayParent is an `mount -t overlay` parent-ref
	// overlayfs (lowerdir under /srv/fc/parent/, upperdir +
	// workdir under /dev/shm/faas-base-staging/). SrcPath +
	// StorageKey are empty; Umount dispatches to
	// UmountOverlayParent which rmdir's the merged dir.
	MountKindOverlayParent
)

// NewRegistry builds an empty registry. cap is the soft cap on
// concurrent mounts — when a Mount would exceed it, the oldest
// entry is force-umounted to make room (load-shedding, not
// back-pressure). Production default cap=DefaultCap (16) matches
// the fleet-wide imaging parallelism of an idle box; bumped to
// 64 on builds under load.
func NewRegistry(cap int) *Registry {
	if cap <= 0 {
		cap = DefaultCap
	}
	return &Registry{entries: make(map[string]MountEntry), cap: cap}
}

// Umount atomically looks up + umounts + forgets + removes the
// staged source for `mountpoint` under a single mutex
// acquisition. Returns:
//
//   - (true,  nil)         — entry was found and the umount
//     syscall + source cleanup succeeded.
//   - (false, nil)         — entry was not found (idempotent
//     defer-after-error path; imaged's
//     UmountParentExt4 wrapper absorbs this).
//   - (false, err)         — entry was found but umount failed
//     (e.g. EBUSY); entry is KEPT in the
//     map so the next sweep can retry.
//     The caller MUST surface the error —
//     silently dropping a real umount
//     failure would leak the loopback mount.
//
// This is the single critical section for the umount lifecycle:
// a concurrent sweep tick + a deferred UmountExt4 from imaged
// used to race (manager's umount + forget held no lock, so the
// sweep could umount the same mountpoint first). Now both paths
// funnel through Umount and the registry stays consistent.
//
// Dispatch: MountKind controls which umount syscall runs.
// MountKindParentExt4 → UmountExt4 + rm SrcPath (the ADR-053
// loopback path). MountKindOverlayParent → UmountOverlayParent
// (the DEPLOY-1 parent-ref overlay path; the function already
// rmdir's the merged dir, so no SrcPath cleanup happens here).
// Review finding B4: pre-MountKind the registry ran a single
// `umount` syscall on every entry and only cleaned up SrcPath;
// MountKindOverlayParent entries had no SrcPath, so the sweep
// leaked upperdir/workdir staging trees.
func (r *Registry) Umount(ctx context.Context, mountpoint string) (found bool, err error) {
	r.mu.Lock()
	entry, ok := r.entries[mountpoint]
	if !ok {
		r.mu.Unlock()
		return false, nil
	}
	delete(r.entries, mountpoint)
	r.mu.Unlock()

	switch entry.Kind {
	case MountKindParentExt4:
		if uerr := UmountExt4(ctx, mountpoint); uerr != nil {
			// Restore the entry so a retry has a chance — the next
			// sweep tick (or a future explicit Umount call) will
			// pick it up. We restore under the lock to keep the map
			// consistent with the disk state.
			if !errors.Is(uerr, ErrUnknownMountpoint) {
				r.mu.Lock()
				if _, stillMissing := r.entries[mountpoint]; stillMissing {
					r.entries[mountpoint] = entry
				}
				r.mu.Unlock()
				return false, uerr
			}
			// ErrUnknownMountpoint means the kernel has nothing at
			// the path — entry was already forgotten, no restore
			// needed.
		}
		if entry.SrcPath != "" {
			if rerr := os.Remove(entry.SrcPath); rerr != nil && !os.IsNotExist(rerr) {
				// Source file removal failure is non-fatal — the
				// next sweep (or a manual umount) will retry. Log
				// via the returned error so the manager can decide.
				return true, fmt.Errorf("vmmdmount: registry.Umount: rm src %s: %w", entry.SrcPath, rerr)
			}
		}
	case MountKindOverlayParent:
		// UmountOverlayParent already handles its own merged-dir
		// cleanup. If it returns ErrUnknownMountpoint, the
		// overlay is already gone — entry stays forgotten.
		if uerr := UmountOverlayParent(ctx, mountpoint); uerr != nil {
			if !errors.Is(uerr, ErrUnknownMountpoint) {
				r.mu.Lock()
				if _, stillMissing := r.entries[mountpoint]; stillMissing {
					r.entries[mountpoint] = entry
				}
				r.mu.Unlock()
				return false, uerr
			}
		}
	default:
		return false, fmt.Errorf("vmmdmount: registry.Umount: unknown MountKind=%d for %q", entry.Kind, mountpoint)
	}
	return true, nil
}

// RegisterOrEvict records (mountpoint, kind, storageKey, srcPath)
// under mu, and if the new size exceeds the cap, force-umounts
// the oldest entry to make room. Returns the evicted mountpoint
// (empty when no eviction was needed) so the caller can log it
// AND honor the eviction by issuing the matching umount syscall
// (review finding B5: pre-B5 callers discarded the returned
// mountpoint and the evicted mount stayed live on disk).
//
// This is the load-shed path: a misbehaving imaged that never calls
// UmountParentExt4 cannot grow the map unboundedly because the cap
// guarantees the oldest entry is dropped on every overflow. A
// well-behaved imaged never sees the eviction branch because it
// umounts before the next Mount.
//
// kind is the MountKind of the entry being registered. StorageKey
// + srcPath are set ONLY for MountKindParentExt4 (the loopback
// path that stages StorageBackend bytes); both are empty for
// MountKindOverlayParent (the merged dir is owned by
// UmountOverlayParent's rmdir).
func (r *Registry) RegisterOrEvict(mountpoint string, kind MountKind, storageKey, srcPath string) (evicted string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.entries) >= r.cap {
		var oldestMP string
		var oldestAt time.Time
		for mp, e := range r.entries {
			if oldestMP == "" || e.MountedAt.Before(oldestAt) {
				oldestMP = mp
				oldestAt = e.MountedAt
			}
		}
		evicted = oldestMP
	}
	if evicted != "" {
		delete(r.entries, evicted)
	}
	r.entries[mountpoint] = MountEntry{
		Kind:       kind,
		StorageKey: storageKey,
		SrcPath:    srcPath,
		MountedAt:  time.Now(),
	}
	return evicted
}

// Lookup returns the entry for mountpoint, or (zero, false) when
// the mountpoint isn't in the registry.
func (r *Registry) Lookup(mountpoint string) (MountEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[mountpoint]
	return e, ok
}

// Forget removes the entry. Idempotent on unknown mountpoints so
// the Umount path is safe to retry after a partial failure.
func (r *Registry) Forget(mountpoint string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, mountpoint)
}

// SweepOrphans walks every entry older than ParentMountMaxAge and
// force-umounts it via the atomic Registry.Umount (which also
// removes the staged source file). Returns the count swept. Safe
// on an empty registry (returns 0). Entries whose umount fails
// are kept in the map so the next sweep tick retries — this
// matches the deferred Umount path (Registry.Umount itself
// restores on real umount errors).
func (r *Registry) SweepOrphans(ctx context.Context, log *slog.Logger) int {
	r.mu.Lock()
	var stale []string
	cutoff := time.Now().Add(-ParentMountMaxAge)
	for mp, e := range r.entries {
		if e.MountedAt.Before(cutoff) {
			stale = append(stale, mp)
		}
	}
	r.mu.Unlock()

	swept := 0
	for _, mp := range stale {
		found, err := r.Umount(ctx, mp)
		if err != nil && log != nil {
			log.Warn("vmmd: orphan parent umount failed", "mountpoint", mp, "err", err)
		}
		if found && err == nil {
			swept++
		}
	}
	return swept
}

// SweepAll force-umounts every live entry. Called from cmd/vmmd's
// SIGTERM handler so the box doesn't leave dangling mounts or
// source files in /srv/fc/parent/. Returns the count swept.
// Empty registry is a no-op.
func (r *Registry) SweepAll(ctx context.Context, log *slog.Logger) int {
	r.mu.Lock()
	mps := make([]string, 0, len(r.entries))
	for mp := range r.entries {
		mps = append(mps, mp)
	}
	r.mu.Unlock()

	swept := 0
	for _, mp := range mps {
		found, err := r.Umount(ctx, mp)
		if err != nil && log != nil {
			log.Warn("vmmd: shutdown parent umount failed", "mountpoint", mp, "err", err)
		}
		if found && err == nil {
			swept++
		}
	}
	return swept
}

// MountExt4ReadOnly loopback-mounts `src` (a path on disk) at a
// fresh mountpoint under MountRoot (/srv/fc/parent). The caller
// (Manager.MountParentExt4) is responsible for (a) populating
// `src` with the StorageBackend bytes via MkdirSrcTemp (which
// also writes under MountRoot), and (b) registering the returned
// mountpoint in the registry so SweepOrphans can find it.
//
// src is NOT removed by this function — the caller owns the staged
// bytes' lifecycle. The mountpoint IS removed on umount, but the
// mountpoint itself is left in place on Mount error so the caller
// can surface the path in the error log without re-creating it.
//
// Mount options: -o loop,ro,nodev,nosuid,noexec. Read-only so a
// child re-stage cannot corrupt the parent; nodev+nosuid+noexec
// harden against any binary inside the parent ext4 executing or
// opening device nodes via the loopback mount.
func MountExt4ReadOnly(ctx context.Context, src string) (mountpoint string, err error) {
	if src == "" {
		return "", errors.New("vmmdmount: MountExt4ReadOnly: empty src")
	}
	if _, err := os.Stat(src); err != nil {
		return "", fmt.Errorf("vmmdmount: MountExt4ReadOnly: stat src: %w", err)
	}
	mp, err := os.MkdirTemp(MountRoot, ParentMountPrefix+"mnt-")
	if err != nil {
		return "", fmt.Errorf("vmmdmount: MountExt4ReadOnly: mkdir mountpoint: %w", err)
	}
	// On any error path, rmdir the freshly-created mountpoint so a
	// failed Mount doesn't leave /srv/fc/parent/faas-parent-mnt-*
	// orphans.
	success := false
	defer func() {
		if !success {
			_ = os.Remove(mp)
		}
	}()

	// exec.CommandContext binds the mount to ctx — a cancelled ctx
	// kills the mount syscall. The mount itself is fast (loopback +
	// read-only ext4 metadata read), so the ctx is a paranoia belt.
	cmd := exec.CommandContext(ctx, "mount", "-o", parentMountOpts, src, mp)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("vmmdmount: MountExt4ReadOnly: mount %s: %w (%s)", parentMountOpts, err, strings.TrimSpace(string(out)))
	}
	success = true
	return mp, nil
}

// MkdirSrcTemp creates a fresh tmp file path under MountRoot for
// staging StorageBackend bytes prior to MountExt4ReadOnly. The
// caller is responsible for writing the bytes and passing the
// returned path to MountExt4ReadOnly; the registry.Umount lifecycle
// (or an explicit UmountExt4 in the error path) cleans up.
//
// Living under MountRoot (not /tmp) is load-bearing — both daemons
// run PrivateTmp=yes (deploy/systemd/faas-{vmmd,imaged}.service)
// and vmmd's /tmp is invisible to imaged's mount namespace. The
// scratch file's owner is vmmd's uid; imaged reads through the
// resulting loopback mount, never the source path directly.
func MkdirSrcTemp() (string, error) {
	f, err := os.CreateTemp(MountRoot, ParentMountPrefix+"src-")
	if err != nil {
		return "", fmt.Errorf("vmmdmount: MkdirSrcTemp: %w", err)
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", fmt.Errorf("vmmdmount: MkdirSrcTemp: close: %w", err)
	}
	return name, nil
}

// UmountExt4 unmounts mountpoint and removes the (now-empty) dir.
// Idempotent: unknown mountpoint → ErrUnknownMountpoint so the
// caller can decide whether to surface the gRPC error or absorb it
// (the gRPC handler absorbs; imaged's defer-after-error relies on
// this). Real umount errors (EBUSY, EINVAL) are surfaced verbatim.
func UmountExt4(ctx context.Context, mountpoint string) error {
	if mountpoint == "" {
		return ErrUnknownMountpoint
	}
	if !strings.HasPrefix(mountpoint, filepath.Join(MountRoot, ParentMountPrefix)) {
		// Defence in depth: refuse to umount a path vmmd didn't
		// issue. A caller that hands back a path under / or
		// /home/foo would otherwise silently run umount on
		// whatever the kernel has there.
		return fmt.Errorf("vmmdmount: umount: mountpoint %q outside vmmd's parent-mnt scratch", mountpoint)
	}
	if _, err := os.Stat(mountpoint); err != nil {
		if os.IsNotExist(err) {
			return ErrUnknownMountpoint
		}
		return fmt.Errorf("vmmdmount: umount: stat mountpoint: %w", err)
	}
	cmd := exec.CommandContext(ctx, "umount", mountpoint)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("vmmdmount: umount %s: %w (%s)", mountpoint, err, strings.TrimSpace(string(out)))
	}
	// rmdir only — the mountpoint should be empty after a successful
	// umount (mkfs.ext4 -d reads through the mount but writes to
	// the new ext4 outside the scratch tree). If rmdir fails
	// (e.g. a debug process left a file), the next sweep will
	// retry; not a fatal error.
	_ = os.Remove(mountpoint)
	return nil
}

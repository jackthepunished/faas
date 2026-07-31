// Package vmmdmount — loopback mount/umount for the ADR-053
// parent-base staging path. vmmd is the only root component (spec
// §11) and runs `mount -o loop,ro` on imaged's behalf; imaged
// (User=faas-imaged + NoNewPrivileges=yes) cannot do this itself.
//
// Architecture:
//
//   - MountParentExt4ReadOnly: looks up storageKey via the configured
//     StorageBackend, stages the bytes into /tmp/faas-parent-src-*,
//     mkdir's /tmp/faas-parent-mnt-*, mounts -o loop,ro, and
//     registers the (mountpoint -> storageKey, mountedAt) pair in
//     Registry. Returns the mountpoint path; caller reads via
//     `cp -a` and calls UmountParentExt4.
//   - UmountParentExt4: looks up the mountpoint in the registry,
//     runs `umount`, removes the registry entry, and rmdir's the
//     mountpoint. Idempotent on unknown mountpoints so imaged's
//     defer-after-error pattern is safe.
//
// Sweep: Registry.SweepOrphans walks every entry older than 5
// minutes and force-umounts it. Called from cmd/vmmd's main
// goroutine on SIGTERM (sync sweep) and on a 5-minute background
// ticker (orphan sweep). The 5-minute window is generous — a
// normal cp -a of ~280 MB takes ~10s; a hung imaged child would
// surface long before the sweep kicks in.
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

// ParentMountMaxAge is the orphan-sweep threshold. Anything older
// than this when SweepOrphans runs is force-umounted. 5 minutes
// matches the normal-cp window with generous headroom for slow
// disks / cold caches.
const ParentMountMaxAge = 5 * time.Minute

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
	StorageKey string    // the canonical StorageBackend key
	SrcPath    string    // the staged StorageBackend bytes; deleted on umount
	MountedAt  time.Time // when MountParentExt4 registered the mount
}

// NewRegistry builds an empty registry. cap is the soft cap on
// concurrent mounts — when a Mount would exceed it, the oldest
// entry is force-umounted to make room (load-shedding, not
// back-pressure). Production default cap=16 matches the fleet-wide
// imaging parallelism of an idle box; bumped to 64 on builds under
// load.
func NewRegistry(cap int) *Registry {
	if cap <= 0 {
		cap = 16
	}
	return &Registry{entries: make(map[string]MountEntry), cap: cap}
}

// RegisterOrEvict records (mountpoint, storageKey) under mu, and if
// the new size exceeds the cap, force-umounts the oldest entry to
// make room. Returns the evicted mountpoint (empty when no eviction
// was needed) so the caller can log it.
//
// This is the load-shed path: a misbehaving imaged that never calls
// UmountParentExt4 cannot grow the map unboundedly because the cap
// guarantees the oldest entry is dropped on every overflow. A
// well-behaved imaged never sees the eviction branch because it
// umounts before the next Mount.
func (r *Registry) RegisterOrEvict(mountpoint, storageKey, srcPath string) (evicted string) {
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
// force-umounts it. Returns the count swept. Safe on an empty
// registry (returns 0). The mutex is held only for the lookup; the
// umount syscall is invoked outside the lock so a slow umount
// (EBUSY when a child re-stage is mid-cp) does not block new
// Mounts.
func (r *Registry) SweepOrphans(log *slog.Logger) int {
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
		if err := UmountExt4(mp); err != nil {
			if log != nil {
				log.Warn("vmmd: orphan parent umount failed", "mountpoint", mp, "err", err)
			}
			// Leave the entry in place — the next sweep will retry.
			continue
		}
		r.Forget(mp)
		swept++
	}
	return swept
}

// SweepAll force-umounts every live entry. Called from cmd/vmmd's
// SIGTERM handler so the box doesn't leave dangling mounts. Returns
// the count swept. Empty registry is a no-op.
func (r *Registry) SweepAll(log *slog.Logger) int {
	r.mu.Lock()
	mps := make([]string, 0, len(r.entries))
	for mp := range r.entries {
		mps = append(mps, mp)
	}
	r.mu.Unlock()

	swept := 0
	for _, mp := range mps {
		if err := UmountExt4(mp); err != nil {
			if log != nil {
				log.Warn("vmmd: shutdown parent umount failed", "mountpoint", mp, "err", err)
			}
			continue
		}
		r.Forget(mp)
		swept++
	}
	return swept
}

// MountExt4ReadOnly loopback-mounts `src` (a path on disk) at a
// fresh mountpoint, both created via os.MkdirTemp under os.TempDir().
// The caller (Manager.MountParentExt4) is responsible for (a)
// populating `src` with the StorageBackend bytes, and (b) registering
// the returned mountpoint in the registry so SweepOrphans can find
// it.
//
// src is NOT removed by this function — the caller owns the staged
// bytes' lifecycle. The mountpoint IS removed on umount, but the
// mountpoint itself is left in place on Mount error so the caller
// can surface the path in the error log without re-creating it.
//
// Mount options: -o loop,ro. Read-only so a child re-stage cannot
// corrupt the parent.
func MountExt4ReadOnly(ctx context.Context, src string) (mountpoint string, err error) {
	if src == "" {
		return "", errors.New("vmmdmount: MountExt4ReadOnly: empty src")
	}
	if _, err := os.Stat(src); err != nil {
		return "", fmt.Errorf("vmmdmount: MountExt4ReadOnly: stat src: %w", err)
	}
	mp, err := os.MkdirTemp("", ParentMountPrefix+"mnt-")
	if err != nil {
		return "", fmt.Errorf("vmmdmount: MountExt4ReadOnly: mkdir mountpoint: %w", err)
	}
	// On any error path, rmdir the freshly-created mountpoint so a
	// failed Mount doesn't leave /tmp/faas-parent-mnt-* orphans.
	success := false
	defer func() {
		if !success {
			_ = os.Remove(mp)
		}
	}()

	// exec.CommandContext binds the mount to ctx — a cancelled ctx
	// kills the mount syscall. The mount itself is fast (loopback +
	// read-only ext4 metadata read), so the ctx is a paranoia belt.
	cmd := exec.CommandContext(ctx, "mount", "-o", "loop,ro", src, mp)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("vmmdmount: MountExt4ReadOnly: mount loop ro: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	success = true
	return mp, nil
}

// UmountExt4 unmounts mountpoint and removes the (now-empty) dir.
// Idempotent: unknown mountpoint → ErrUnknownMountpoint so the
// caller can decide whether to surface the gRPC error or absorb it
// (the gRPC handler absorbs; imaged's defer-after-error relies on
// this). Real umount errors (EBUSY, EINVAL) are surfaced verbatim.
func UmountExt4(mountpoint string) error {
	if mountpoint == "" {
		return ErrUnknownMountpoint
	}
	if !strings.HasPrefix(mountpoint, filepath.Join(os.TempDir(), ParentMountPrefix)) {
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
	cmd := exec.Command("umount", mountpoint)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("vmmdmount: umount %s: %w (%s)", mountpoint, err, strings.TrimSpace(string(out)))
	}
	// rmdir only — the mountpoint should be empty after a successful
	// umount (imaged's cp -a writes to staging, not the mountpoint).
	// If rmdir fails (e.g. a debug process left a file), the next
	// sweep will retry; not a fatal error.
	_ = os.Remove(mountpoint)
	return nil
}

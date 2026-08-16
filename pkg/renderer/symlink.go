package renderer

import (
	"fmt"
	"os"
	"path/filepath"
)

// installCurrentSymlink creates or refreshes the `/opt/faas/current`
// symlink pointing at the active release directory. Mirrors
// `pkg/releaseinstall.AtomicFlip` (PR-3) but does NOT import that
// package — pkg/releaseinstall owns the daemon-binary bundle
// lifecycle, pkg/renderer owns the per-host artifact publish. The
// two cooperate via the filesystem but do not share a Go import.
//
// The flow:
//
//  1. If currentPath already points at target, return (no-op).
//  2. Otherwise, create a staging symlink at
//     `<parent>/.current.tmp → target` and rename it over currentPath.
//     os.Rename is POSIX-atomic on the same filesystem.
//
// On error mid-flight, the staging symlink is removed so a subsequent
// run can recover (the same shape as pkg/releaseinstall).
func installCurrentSymlink(currentPath, target string) error {
	// Step 1: idempotent short-circuit.
	if existing, err := os.Readlink(currentPath); err == nil {
		if existing == target {
			return nil
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("renderer: readlink %s: %w", currentPath, err)
	}

	// Step 2: stage + rename.
	parent := filepath.Dir(currentPath)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("renderer: mkdir %s: %w", parent, err)
	}

	staging := filepath.Join(parent, ".current.tmp")
	if err := os.Remove(staging); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("renderer: clean staging %s: %w", staging, err)
	}
	if err := os.Symlink(target, staging); err != nil {
		return fmt.Errorf("renderer: create staging %s: %w", staging, err)
	}
	if err := os.Rename(staging, currentPath); err != nil {
		_ = os.Remove(staging)
		return fmt.Errorf("renderer: rename staging → %s: %w", currentPath, err)
	}
	return nil
}

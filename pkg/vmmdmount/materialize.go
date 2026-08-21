package vmmdmount

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// MaterializeParentExt4 copies a mounted parent tree into a shared staging
// directory.  Mounts created in vmmd's systemd namespace are not a reliable
// file-view boundary for the unprivileged imaged process, so the root-owned
// copy is the explicit handoff between the two daemons.
func MaterializeParentExt4(ctx context.Context, lowerdir, targetDir string) error {
	if lowerdir == "" || targetDir == "" {
		return fmt.Errorf("vmmdmount: materialize parent: empty path")
	}
	if err := rejectSymlinkOrEscape(lowerdir, filepath.Clean(MountRoot)+string(filepath.Separator), "lowerdir"); err != nil {
		return err
	}
	stagingPrefix := filepath.Clean(OverlayStagingRoot) + string(filepath.Separator)
	if err := rejectSymlinkOrEscape(targetDir, stagingPrefix, "target_dir"); err != nil {
		return err
	}
	if info, err := os.Stat(lowerdir); err != nil {
		return fmt.Errorf("vmmdmount: materialize parent: stat lowerdir: %w", err)
	} else if !info.IsDir() {
		return fmt.Errorf("vmmdmount: materialize parent: lowerdir %q is not a directory", lowerdir)
	}
	if info, err := os.Stat(targetDir); err != nil {
		return fmt.Errorf("vmmdmount: materialize parent: stat target_dir: %w", err)
	} else if !info.IsDir() {
		return fmt.Errorf("vmmdmount: materialize parent: target_dir %q is not a directory", targetDir)
	}

	return copyParentTree(ctx, lowerdir, targetDir)
}

// copyParentTree is kept separate so the content-vs-directory copy contract
// is testable without requiring a real loopback mount or privileged paths.
func copyParentTree(ctx context.Context, lowerdir, targetDir string) error {
	// cp -a preserves symlinks, modes, ownership, timestamps, and sparse
	// files from the Debian parent. The target is created by imaged and is
	// intentionally shared under /dev/shm so the unprivileged caller can
	// apply the child OCI layers and run mkfs.ext4 -d afterwards.
	// Keep the trailing `/.` intact. filepath.Join(lowerdir, ".") cleans it
	// back to lowerdir, which makes cp place the whole parent directory under
	// targetDir instead of copying the parent's contents.
	src := lowerdir + string(filepath.Separator) + "."
	cmd := exec.CommandContext(ctx, "cp", "-a", src, targetDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("vmmdmount: materialize parent: cp %q -> %q: %w (%s)",
			lowerdir, targetDir, err, strings.TrimSpace(string(out)))
	}
	return nil
}

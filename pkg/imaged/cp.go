// Package imaged — cp.go: tiny `cp -a` wrapper used by the
// ADR-053 parent-base staging path. imaged is non-root (User=
// faas-imaged, NoNewPrivileges=yes per spec §11) and cannot
// loopback-mount the parent ext4 itself; vmmd runs `mount -o
// loop,ro` on imaged's behalf and returns the mountpoint path.
// imaged then `cp -a <mp>/. <staging>` to materialise the parent
// tree into a fresh staging dir, applies the delta OCI layers on
// top, and hands the staging tree to BuildBaseFromStaging for
// mkfs.
//
// Why `cp -a` and not os.CopyFS: cp preserves every file
// attribute (mode, ownership, symlinks, xattrs) without
// re-implementing them in Go. For the ADR-053 staging path we
// specifically need ownership preservation (the parent's
// files are owned by root with various uids/gids; flattening
// to 0o644 would silently break any SUID/SGID bits the
// debian:12-slim userland relies on). cp -a is the smallest
// tool that does the right thing.
//
// src must end in "/." — the cp idiom for "copy contents, not
// the directory itself". This is what makes
// `cp -a <mp>/. <staging>` produce a staging dir that mirrors
// the parent ext4's root layout (/bin, /lib, /usr, …) rather
// than nesting everything under /<mp-basename>/.
package imaged

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// CopyTree runs `cp -a src/. dst` via exec.CommandContext, so a
// cancelled parent ctx kills the copy mid-flight. Returns the
// combined stdout/stderr on non-zero exit so the caller can
// surface the failure mode (e.g. "Permission denied" when the
// mount is read-only and a delta layer tried to write to a
// read-only file).
//
// src must end in "/." — the cp idiom for "copy contents". The
// caller (the parent-ref branch of EnsureBaseExt4) passes
// <mountpoint+"/."> so the staging dir mirrors the parent ext4's
// root layout, not the mountpoint basename.
func CopyTree(ctx context.Context, src, dst string) error {
	if src == "" || dst == "" {
		return fmt.Errorf("imaged: CopyTree: empty src or dst")
	}
	if !strings.HasSuffix(src, "/.") {
		return fmt.Errorf("imaged: CopyTree: src %q must end in %q (the cp idiom for 'copy contents')", src, "/.")
	}
	cmd := exec.CommandContext(ctx, "cp", "-a", src, dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("imaged: cp -a %s %s: %w (%s)", src, dst, err, strings.TrimSpace(string(out)))
	}
	return nil
}

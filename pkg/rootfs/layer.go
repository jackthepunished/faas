package rootfs

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ApplyLayer unpacks one OCI/Docker layer (an uncompressed tar) into dst,
// applying it on top of whatever earlier layers already populated dst. Layers
// must be applied bottom-to-top. It handles aufs-style whiteouts and refuses any
// entry whose path would escape dst (path traversal is a build-input attack
// surface, spec §9.1).
//
// Note: whiteouts here delete from the staging tree, which is correct for one app
// layer removing a file introduced by a lower app layer. Hiding a file that lives
// in the shared BASE (drive0) requires an overlayfs char-device whiteout created
// at mkfs time under root — tracked separately; the common add-only app never
// hits it.
func ApplyLayer(dst string, tr *tar.Reader) error {
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("rootfs: read tar: %w", err)
		}

		// codeql[go/path-injection] false-positive: resolveEntryPath rejects ".."
		// and absolute names, then clamps every ancestor symlink inside dst.
		target, err := resolveEntryPath(dst, hdr.Name)
		if err != nil {
			return err
		}

		base := filepath.Base(hdr.Name)
		switch {
		case base == whiteoutOpaque:
			// Opaque dir: drop everything currently under its parent.
			if err := clearDir(filepath.Dir(target)); err != nil {
				return err
			}
			continue
		case strings.HasPrefix(base, whiteoutPrefix):
			// Delete the named sibling from lower layers.
			victim := filepath.Join(filepath.Dir(target), strings.TrimPrefix(base, whiteoutPrefix))
			if err := os.RemoveAll(victim); err != nil {
				return fmt.Errorf("rootfs: whiteout %s: %w", victim, err)
			}
			continue
		}

		if err := applyEntry(dst, target, hdr, tr); err != nil {
			return err
		}
	}
}

// ApplyLayerGz applies a gzip-compressed layer.
func ApplyLayerGz(dst string, r io.Reader) error {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("rootfs: gzip: %w", err)
	}
	defer func() { _ = zr.Close() }()
	return ApplyLayer(dst, tar.NewReader(zr))
}

const (
	whiteoutPrefix = ".wh."
	whiteoutOpaque = ".wh..wh..opq"
)

func applyEntry(base, target string, hdr *tar.Header, tr io.Reader) error {
	switch hdr.Typeflag {
	case tar.TypeDir:
		return os.MkdirAll(target, os.FileMode(hdr.Mode)&os.ModePerm)
	case tar.TypeReg:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		// A later OCI layer may replace a symlink from a lower layer with a
		// regular file. Remove the link before opening the destination so
		// OpenFile cannot follow its guest-side target into the host filesystem
		// (for example /bin/busybox under systemd ProtectSystem=strict).
		if info, err := os.Lstat(target); err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				if err := os.Remove(target); err != nil {
					return fmt.Errorf("rootfs: replace symlink %s: %w", target, err)
				}
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("rootfs: inspect existing entry %s: %w", target, err)
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&os.ModePerm)
		if err != nil {
			return err
		}
		// Bound the copy by the declared size to avoid a decompression bomb
		// writing unboundedly.
		if _, err := io.CopyN(f, tr, hdr.Size); err != nil && !errors.Is(err, io.EOF) {
			_ = f.Close()
			return fmt.Errorf("rootfs: write %s: %w", target, err)
		}
		return f.Close()
	case tar.TypeSymlink:
		// A symlink's Linkname is GUEST-side data, not a host path: the
		// string is stored verbatim in the ext4 inode and is resolved by
		// the guest kernel at use time, where the staging root IS "/".
		// So it must be written exactly as the layer declares it —
		// absolute targets stay absolute ("/bin/busybox"), relative
		// targets stay relative ("../lib/foo").
		//
		// Absolute targets are not an edge case: the alpine base layer
		// alone ships 306 of them (every bin/* applet -> /bin/busybox),
		// and so does every Debian/Ubuntu image. Rewriting them into
		// host staging paths — or rejecting them — makes it impossible
		// to build a base rootfs from any real OCI image.
		//
		// This is safe because the host never *follows* these links:
		// resolveEntryPath clamps ancestor symlinks inside dst on every
		// subsequent entry, so a hostile "bin -> /etc" link cannot be
		// used to write through to the host's /etc. See resolveWithin.
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		_ = os.Remove(target)
		return os.Symlink(hdr.Linkname, target)
	case tar.TypeLink:
		// A hardlink's Linkname IS resolved host-side (os.Link needs a
		// real path), and per tar semantics it is relative to the
		// archive root even when written with a leading "/".
		source, err := resolveLinkSource(base, hdr.Linkname)
		if err != nil {
			return err
		}
		_ = os.Remove(target)
		return os.Link(source, target)
	default:
		// Char/block/fifo devices are not expected in app layers; skip them
		// rather than fail the whole build.
		return nil
	}
}

// safeJoin joins name onto base and guarantees the result stays within base,
// REJECTING (not silently clamping) absolute paths and ".." traversal — a
// malicious or broken layer must fail the build, not be quietly neutralised
// (spec §9.1).
//
// safeJoin is purely SYNTACTIC: it says nothing about symlinks already on
// disk. It is the first of two gates; resolveWithin is the second. It applies
// to tar entry NAMES (and hardlink sources), never to symlink Linknames —
// those are guest-side strings, see applyEntry's TypeSymlink branch.
func safeJoin(base, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("rootfs: empty entry name")
	}
	if strings.HasPrefix(name, "/") || filepath.IsAbs(name) {
		return "", fmt.Errorf("rootfs: absolute entry path %q rejected", name)
	}
	clean := filepath.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("rootfs: entry %q escapes staging root", name)
	}
	joined := filepath.Join(base, clean)
	// Defence in depth: confirm the final path is still under base.
	rel, err := filepath.Rel(base, joined)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("rootfs: entry %q escapes staging root", name)
	}
	return joined, nil
}

// maxSymlinkHops bounds symlink chasing in resolveWithin, mirroring the
// kernel's SYMLOOP_MAX. A layer containing a symlink cycle ("a -> b",
// "b -> a") must fail the build, not spin forever.
const maxSymlinkHops = 40

// resolveEntryPath computes the host path an entry named `name` is written
// to, given staging root `root`.
//
// The final component is deliberately NOT symlink-resolved: an OCI layer
// entry for path X REPLACES whatever sits at X (overlayfs semantics), so a
// regular-file entry landing on an existing symlink must clobber the link,
// not write through it. Every ANCESTOR component is resolved and clamped
// inside root by resolveWithin.
func resolveEntryPath(root, name string) (string, error) {
	// Gate 1 (syntactic): reject absolute names and ".." traversal.
	if _, err := safeJoin(root, name); err != nil {
		return "", err
	}
	clean := filepath.Clean(name)
	if clean == "." {
		return root, nil
	}
	// Gate 2 (on-disk): resolve the parent, clamping ancestor symlinks.
	parent, err := resolveWithin(root, filepath.Dir(clean))
	if err != nil {
		return "", fmt.Errorf("rootfs: entry %q: %w", name, err)
	}
	return filepath.Join(parent, filepath.Base(clean)), nil
}

// resolveLinkSource resolves a hardlink's Linkname to a host path. Unlike a
// symlink target, a hardlink source must exist on the host right now for
// os.Link to succeed. Per tar semantics the name is relative to the archive
// root even when a producer writes it with a leading "/", so strip that
// first rather than rejecting it.
func resolveLinkSource(root, linkname string) (string, error) {
	rel := strings.TrimPrefix(linkname, "/")
	if _, err := safeJoin(root, rel); err != nil {
		return "", err
	}
	// Hardlink sources resolve fully — including the last component, which
	// must name a real existing file.
	resolved, err := resolveWithin(root, rel)
	if err != nil {
		return "", fmt.Errorf("rootfs: hardlink %q: %w", linkname, err)
	}
	return resolved, nil
}

// resolveWithin walks `rel` component-by-component beneath `root`, following
// symlinks encountered along the way but CLAMPING them inside root — the
// same contract as a chroot, and what containerd/moby do when unpacking
// layers.
//
// Clamping (rather than rejecting) is what makes it safe to store symlink
// Linknames verbatim. A hostile layer that ships "bin -> /etc" followed by
// "bin/passwd" gets the link written literally, but this walk resolves the
// later write to <root>/etc/passwd — inside staging. The host's /etc is
// unreachable. Rejecting instead would break legitimate images: Debian's
// merged-usr layout ("bin -> usr/bin") relies on exactly this traversal.
//
// TOCTOU note: this is a non-atomic lstat walk, which is sound HERE because
// the staging tree is a fresh os.MkdirTemp written by a single goroutine of
// this daemon. The attacker controls tar bytes, not concurrent filesystem
// access. If staging ever becomes shared or concurrently written, this must
// move to openat2(RESOLVE_IN_ROOT).
func resolveWithin(root, rel string) (string, error) {
	cur := root
	// Remaining components to consume, innermost-first.
	todo := splitPath(rel)
	for hops := 0; len(todo) > 0; {
		comp := todo[0]
		todo = todo[1:]

		switch comp {
		case "", ".":
			continue
		case "..":
			// Clamp at root: ".." from the top stays at the top.
			if cur != root {
				cur = filepath.Dir(cur)
			}
			continue
		}

		next := filepath.Join(cur, comp)
		fi, err := os.Lstat(next)
		if err != nil {
			if os.IsNotExist(err) {
				// Nothing on disk yet — the caller will MkdirAll it.
				cur = next
				continue
			}
			return "", err
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			cur = next
			continue
		}

		hops++
		if hops > maxSymlinkHops {
			return "", fmt.Errorf("too many symlink hops resolving %q", rel)
		}
		dest, err := os.Readlink(next)
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(dest) {
			// Absolute link target: re-anchor at root, NOT at host "/".
			// This is the clamp that makes verbatim Linknames safe.
			cur = root
		}
		todo = append(splitPath(dest), todo...)
	}
	return cur, nil
}

// splitPath splits a slash path into components, dropping empties.
func splitPath(p string) []string {
	parts := strings.Split(filepath.ToSlash(p), "/")
	out := parts[:0]
	for _, s := range parts {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// clearDir removes every child of dir but keeps dir itself.
func clearDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

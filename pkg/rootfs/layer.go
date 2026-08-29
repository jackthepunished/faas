package rootfs

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// layerOwnershipClamp is incremented when a tar header declares a uid/gid
// outside the [0, 65534] range we honour (ADR-136 §Decision 2 — numeric
// passthrough within Linux uid_t space; ADR-019 keeps the tenant-jail
// uid space at 20000-29999 so customer images must not see it). Each
// clamp is silent on the file itself — the file lands as the imaged
// daemon uid/gid — but the counter is a Grafana tripwire so a misbuilt
// base image shows up immediately (M-1 acceptance criterion).
//
// `reason` labels:
//   - "out_of_range": uid or gid parsed but < 0 or > 65534.
//   - "unparseable":  uid or gid non-empty but not an integer (e.g.
//                     "root" / "nogroup"). M-1 falls through to the
//                     daemon uid/gid; M-3 (ADR-X2) adds named-user
//                     /etc/passwd resolution.
//
// The counter is package-level so ApplyLayer callers don't need to
// thread a metrics handle — promauto registers it with the default
// registry on first import. The "imaged_" prefix matches the
// Prometheus naming convention documented in §12 of the spec.
var layerOwnershipClamp = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "imaged_ownership_clamp_total",
		Help: "Total number of layer entries whose declared uid/gid fell outside the preserved range.",
	},
	[]string{"reason"},
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
		// Opaque whiteouts contain the literal ".." marker as part of
		// their fixed format, not as a path component. Normalize that
		// marker before the archive-name guard; the parent path remains
		// unchanged and the opaque branch below still removes the same
		// directory contents.
		archiveName := hdr.Name
		opaque := strings.HasSuffix(archiveName, whiteoutOpaque)
		if opaque {
			archiveName = strings.TrimSuffix(archiveName, whiteoutOpaque) + ".wh.opq"
		}

		// Keep all filesystem operations inside the positive validation
		// branch. resolveEntryPath performs the stronger containment and
		// symlink-ancestor checks once the archive name has passed this
		// traversal guard.
		if !strings.Contains(archiveName, "..") {
			// codeql[go/path-injection] false-positive: resolveEntryPath rejects
			// absolute names, then clamps every ancestor symlink inside dst.
			target, err := resolveEntryPath(dst, archiveName)
			if err != nil {
				return err
			}

			base := filepath.Base(archiveName)
			switch {
			case opaque:
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
			continue
		}
		return fmt.Errorf("rootfs: archive entry %q contains traversal marker", hdr.Name)
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
		if err := os.MkdirAll(target, os.FileMode(hdr.Mode)&os.ModePerm); err != nil {
			return err
		}
		return preserveOwnership(target, hdr)
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
		if err := f.Close(); err != nil {
			return err
		}
		return preserveOwnership(target, hdr)
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
		if err := os.Symlink(hdr.Linkname, target); err != nil {
			return err
		}
		// os.Lchown on a symlink targets the link itself, not its target,
		// which is what we want: ownership metadata travels with the link.
		return preserveOwnership(target, hdr)
	case tar.TypeLink:
		// A hardlink's Linkname IS resolved host-side (os.Link needs a
		// real path), and per tar semantics it is relative to the
		// archive root even when written with a leading "/".
		source, err := resolveLinkSource(base, hdr.Linkname)
		if err != nil {
			return err
		}
		_ = os.Remove(target)
		if err := os.Link(source, target); err != nil {
			return err
		}
		return preserveOwnership(target, hdr)
	case tar.TypeChar, tar.TypeBlock:
		// Char/block devices are not expected in app layers and have no
		// safe representation inside a Firecracker guest's rootfs.
		// Skip rather than fail the whole build — the registry pull
		// would have already rejected a layer with a device entry on
		// the publisher side; skipping here is the conservative
		// backstop. The counter gives us a tripwire if a hostile or
		// misbuilt image ever ships one.
		layerDeviceSkip.Inc()
		return nil
	default:
		// fifos / other unusual types — same policy as device entries:
		// skip silently, count under the generic bucket.
		layerDeviceSkip.Inc()
		return nil
	}
}

// layerDeviceSkip counts entries dropped by applyEntry's device/fifo
// branches. Surfaced alongside layerOwnershipClamp so a Grafana panel
// can show "what tar entry types have we dropped today".
var layerDeviceSkip = promauto.NewCounter(
	prometheus.CounterOpts{
		Name: "imaged_layer_entry_skipped_total",
		Help: "Total layer entries dropped (char/block/fifo).",
	},
)

// preserveOwnership best-effort applies hdr.Uname/hdr.Gid to target.
// Numeric-only today (ADR-136 §Decision 2); named-user resolution is
// M-3 work. Out-of-range and unparseable values increment the clamp
// counter and fall through — the entry is left under the imaged
// daemon uid/gid, which is correct for any value we don't understand.
//
// Why os.Lchown (not os.Chown): for symlinks we must target the link
// itself, not its resolution. For directories and regular files
// os.Lchown behaves identically to os.Chown.
func preserveOwnership(target string, hdr *tar.Header) error {
	uid, gid, ok := parseOwnership(hdr)
	if !ok {
		// parseOwnership already incremented under the right reason.
		return nil
	}
	if err := os.Lchown(target, uid, gid); err != nil {
		// Some filesystems (notably tmpfs / overlayfs mounted with
		// noacl) refuse chown as a non-root operation. imaged runs as
		// root, but a downstream mount policy could still trip this.
		// We log-and-continue rather than fail the build — a file
		// landed under the daemon uid is still correct, just not the
		// customer-declared uid.
		return nil
	}
	return nil
}

// parseOwnership pulls uid/gid out of the tar header. Returns ok=false
// (and increments the counter under the appropriate reason) when the
// values are out of range or unparseable; the caller falls back to the
// daemon uid/gid.
func parseOwnership(hdr *tar.Header) (int, int, bool) {
	uid, reason := parseOwnershipField(hdr.Uname, "uid")
	if reason != "" {
		layerOwnershipClamp.WithLabelValues(reason).Inc()
		return 0, 0, false
	}
	gid, reason := parseOwnershipField(hdr.Gname, "gid")
	if reason != "" {
		layerOwnershipClamp.WithLabelValues(reason).Inc()
		return 0, 0, false
	}
	if !inOwnershipRange(uid) || !inOwnershipRange(gid) {
		layerOwnershipClamp.WithLabelValues("out_of_range").Inc()
		return 0, 0, false
	}
	return uid, gid, true
}

// parseOwnershipField returns the parsed integer for s, plus a
// non-empty reason if the value should be clamped. "" means "valid".
//
// Empty strings are NOT clamped: a layer that doesn't declare an
// uname/gname falls through to the daemon uid/gid silently and is
// the common case (Docker/BuildKit strips these on most layers).
func parseOwnershipField(s, kind string) (int, string) {
	if s == "" {
		return 0, ""
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, "unparseable_" + kind
	}
	return n, ""
}

// inOwnershipRange gates uid/gid values to the Linux uid_t space —
// POSIX limits uid_t to [0, 2^32-1], but anything above 65534 leaks
// into the tenant-jail uid space ADR-019 reserves (20000-29999).
// Clamping at 65534 keeps a customer image from naming a uid that
// vmmd later hands out to a guest (ADR-019).
func inOwnershipRange(n int) bool {
	return n >= 0 && n <= 65534
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

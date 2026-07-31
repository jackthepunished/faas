package main

// extract.go — Phase 3 tarball extraction seam.
//
// distinct from cmd/apid/deploy_inputs.go::validateTarballShape, which
// only inspects the gzip header stream and never touches disk. The
// scanner path needs the bytes on disk because reposcan.Scan takes an
// fs.FS (os.DirFS(root)), not an io.Reader. The two helpers coexist:
// `validateAndSpool` runs first (compressed cap, file-count cap,
// symlink-name escape) and `extractTarGzToDir` runs second (total
// expanded cap, per-entry size cap, full entry-type allow-list).
//
// every entry-type rejection is fail-closed because a malicious
// tarball that writes to /dev/null or unlinks /etc/passwd would be
// catastrophic; the package-level cap keeps the disk cost bounded;
// the scrub dir is created with 0o700 and removed via defer.

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/onebox-faas/faas/pkg/api"
)

// scanSpoolRoot returns the directory under which per-request
// extraction scratch dirs are created. env-overridable for tests so
// they can keep the spool under t.TempDir() without touching
// /var/spool/faas.
const scanSpoolRootEnv = "FAAS_SCAN_SPOOL_ROOT"

func scanSpoolRoot() string {
	if v := os.Getenv(scanSpoolRootEnv); v != "" {
		return v
	}
	return "/var/spool/faas/scans"
}

// extractLimits caps what extractTarGzToDir will unpack. The defaults
// match the §4 plan cap (SourceTarballMaxMB) × 2.5 for the expanded
// total (ADR-050 §3). Per-entry cap stops a single 10 GB entry
// inside a small compressed envelope. File-count cap is the same
// 10 000 as the source-deploy path so customers see a consistent
// envelope across the two surfaces.
type extractLimits struct {
	MaxEntries    int   // default 10_000
	MaxFileBytes  int64 // default 256 MiB
	MaxTotalBytes int64 // default = compressed cap × 2.5
}

// defaultExtractLimits returns the limits for a given plan. Both
// compressed and expanded caps live in api.Limits — we scale the
// compressed cap by 2.5× to give customers headroom for already-tar
// sources. The constants are intentionally not literal here so a
// future limit table edit propagates without a sync point.
func defaultExtractLimits(l api.Limits) extractLimits {
	compressed := int64(l.SourceTarballMaxMB) * 1024 * 1024
	return extractLimits{
		MaxEntries:    10_000,
		MaxFileBytes:  256 * 1024 * 1024,
		MaxTotalBytes: compressed*2 + compressed/2, // 2.5x
	}
}

// extractTarGzToDir unpacks src into a freshly-created directory
// under scanSpoolRoot() and returns the dir path. The dir is removed
// by the caller (use defer os.RemoveAll). On error the partial
// directory is cleaned up before returning — no half-unpacked tarballs
// leak to the next request.
//
// Rules (mirrors the §11 hardening posture):
//   - Reject absolute paths and `..` segments (already done by
//     validateTarballShape, but we re-check defensively in case the
//     caller skipped that step).
//   - Reject TypeLink, TypeSymlink, TypeChar, TypeBlock, TypeFifo.
//     A symlink whose target is inside the archive root is the
//     classic "exfil a host file" vector — never allow.
//   - Reject entry counts beyond MaxEntries.
//   - Reject any single entry whose body exceeds MaxFileBytes
//     (read with io.LimitReader so a hostile tarball can't pin
//     apid's memory).
//   - Reject cumulative expanded bytes beyond MaxTotalBytes.
func extractTarGzToDir(src string, lim extractLimits) (string, *api.Problem) {
	if err := os.MkdirAll(scanSpoolRoot(), 0o700); err != nil {
		return "", api.ErrCapacity("could not create scan spool dir")
	}
	id := randomToken(12)
	dst := filepath.Join(scanSpoolRoot(), id)
	if err := os.Mkdir(dst, 0o700); err != nil {
		return "", api.ErrCapacity("could not create scan dir")
	}
	if prob := extractTarGzInto(src, dst, lim); prob != nil {
		_ = os.RemoveAll(dst)
		return "", prob
	}
	return dst, nil
}

func extractTarGzInto(src, dst string, lim extractLimits) *api.Problem {
	f, err := os.Open(src)
	if err != nil {
		return api.NewProblem(http.StatusBadRequest, api.CodeSourceInvalid,
			"Bad source", err.Error())
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return api.NewProblem(http.StatusBadRequest, api.CodeSourceInvalid,
			"Not gzip", "source must be tar.gz")
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)

	var (
		entries int
		total   int64
		// firstDir is the single top-level prefix the tarball must
		// share (tar wraps every entry under `<root>/...`). We
		// strip it on write so reposcan.Scan sees a clean root
		// directory. Set on the first non-empty-name header.
		firstDir string
		firstSet bool
	)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return api.NewProblem(http.StatusBadRequest, api.CodeSourceInvalid,
				"Bad tar", err.Error())
		}
		// Reject every named escape — mirrors escapesArchiveRoot.
		// The tar name walks up the parent dir if joined under any
		// root; we keep the same predicate the deploy path uses so
		// both surfaces fail the same set of inputs.
		if hdr.Name == "" {
			continue
		}
		if escapesArchiveRoot(hdr.Name) {
			return api.ErrSourceInvalid("absolute paths or '..' entries are rejected")
		}
		// Reject every type that could exfiltrate or escape. TypeReg
		// and TypeRegA are the only safe ones; TypeDir is allowed
		// so a tarball can carry directory entries (Railpack-style
		// archives do).
		switch hdr.Typeflag {
		case tar.TypeReg, tar.TypeRegA, tar.TypeDir:
			// allowed
		default:
			return api.ErrSourceInvalid(
				fmt.Sprintf("entry type %d not allowed (only files/dirs)", hdr.Typeflag))
		}
		entries++
		if entries > lim.MaxEntries {
			return api.ErrSourceInvalid(
				fmt.Sprintf("too many files (>%d)", lim.MaxEntries))
		}

		// Strip the leading "<root>/" prefix on the first
		// non-empty-name header. tar archives the customer uploaded
		// usually have this; fs.MapFS test fixtures don't. The
		// no-prefix case (single root already) leaves firstDir="".
		name := hdr.Name
		if !firstSet {
			// Only consume the first segment as the archive root
			// if every other entry also begins with it. We can't
			// know that yet on the first header, so we tentatively
			// set and re-strip; on a mismatch the path becomes
			// root-relative (still safe — escapesArchiveRoot
			// already cleared the security check).
			if i := strings.IndexByte(name, '/'); i >= 0 {
				firstDir = name[:i]
			}
			firstSet = true
		}
		if firstDir != "" && strings.HasPrefix(name, firstDir+"/") {
			name = name[len(firstDir)+1:]
		}
		if name == "" {
			// the root directory entry itself; skip
			continue
		}

		target := filepath.Join(dst, filepath.FromSlash(name))
		// final defensive IsLocal check after Join — catches any
		// input that slipped past the segment-split predicate
		// (Windows path semantics, trailing dots, etc.).
		if !filepath.IsLocal(filepath.Dir(target)) && filepath.Dir(target) != dst {
			return api.ErrSourceInvalid("path escape after join rejected")
		}

		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return api.ErrCapacity("could not create dir")
			}
			continue
		}

		// File: cap per-entry size and cumulative bytes. Read with
		// io.LimitReader so a hostile entry that claims a 10 GiB
		// body can't pin apid.
		if hdr.Size > lim.MaxFileBytes {
			return api.NewProblem(http.StatusRequestEntityTooLarge,
				api.CodeSourceTooLarge,
				"Entry too large",
				fmt.Sprintf("entry %q is %d bytes; cap is %d", name, hdr.Size, lim.MaxFileBytes))
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return api.ErrCapacity("could not create parent dir")
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return api.ErrCapacity("could not create file")
		}
		written, copyErr := io.Copy(out, io.LimitReader(tr, lim.MaxFileBytes))
		_ = out.Close()
		if copyErr != nil {
			return api.NewProblem(http.StatusBadRequest, api.CodeSourceInvalid,
				"Bad tar", copyErr.Error())
		}
		total += written
		if total > lim.MaxTotalBytes {
			return api.NewProblem(http.StatusRequestEntityTooLarge,
				api.CodeSourceTooLarge,
				"Source too large",
				fmt.Sprintf("expanded total exceeds %d bytes", lim.MaxTotalBytes))
		}
	}
	return nil
}

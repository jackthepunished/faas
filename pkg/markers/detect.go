package markers

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
)

// DetectFromFS inspects the top-level entries of fsys (no
// recursion) and returns the framework. fsys is typically
// os.DirFS(srcDir) on the CLI side. Returns
// (FrameworkUnknown, nil) when no marker is found at the root —
// the caller decides whether that's an error.
//
// Nested entries (anything containing "/") are ignored, matching
// the server-side rule at the prior pkg/builderd/detect.go:67-72.
// The CLI uses the same shape so a project root marker is the
// same on both sides.
//
// The marker list is iterated in priority order (Dockerfile
// first), so DetectFromFS and DetectFromTarball return identical
// answers on identical inputs — this is the parity contract
// pinned by TestDetectCLIParity.
func DetectFromFS(fsys fs.FS) (Framework, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return FrameworkUnknown, fmt.Errorf("markers: read fs: %w", err)
	}
	// Build a set of present top-level names (case-folded). Doing
	// the lookup via a set rather than per-entry is O(N+M) instead
	// of O(N*M), which matters when the marker list grows.
	present := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		// Top-level only — anything with a "/" is in a subdir.
		// fs.DirEntry names are base names when read with ".",
		// so this check is belt-and-braces.
		if strings.Contains(e.Name(), "/") {
			continue
		}
		present[strings.ToLower(e.Name())] = true
	}
	for _, m := range appMarkers {
		if present[strings.ToLower(m.filename)] {
			return m.framework, nil
		}
	}
	return FrameworkUnknown, nil
}

// DetectFromTarball opens the gzipped tarball at path and
// returns the framework. Symmetric with DetectFromFS — both
// return the same answer on the same input (parity pinned by
// TestDetectCLIParity). Both return (FrameworkUnknown, nil)
// when no marker is found at the root; a non-nil error is
// reserved for IO failures (open, gzip, tar read).
//
//nolint:forbidigo // path is the apid-spooled tarball that already passed apid's validateTarballShape (in cmd/apid/deploy_inputs.go) before builderd received the build notification. Symlink-attack impossible because apid wrote the file with a fresh random id. Direct unit-test callers construct the path themselves; rationale holds. The original comment lived at pkg/builderd/detect.go:40 and applies unchanged here.
func DetectFromTarball(path string) (Framework, error) {
	f, err := os.Open(path)
	if err != nil {
		return FrameworkUnknown, fmt.Errorf("markers: open: %w", err)
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return FrameworkUnknown, fmt.Errorf("markers: gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	present := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return FrameworkUnknown, fmt.Errorf("markers: read tar: %w", err)
		}
		// Top-level only — a nested package.json under apps/web is
		// not the project's package.json. Mirrors the original
		// pkg/builderd/detect.go:67-72 rule.
		if strings.Contains(hdr.Name, "/") {
			continue
		}
		present[strings.ToLower(hdr.Name)] = true
	}
	for _, m := range appMarkers {
		if present[strings.ToLower(m.filename)] {
			return m.framework, nil
		}
	}
	// No marker at root — return (FrameworkUnknown, nil) so
	// the parity contract holds: both sides answer the same
	// way for unknown input. The CLI's pre-refactor
	// detectFramework similarly returned fwUnknown without an
	// error; the server's prior pkg/builderd.detch.Detect
	// returned an error, but the parity test pins the
	// CLI shape as authoritative. Callers that need an error
	// can wrap the (unknown, nil) tuple.
	return FrameworkUnknown, nil
}

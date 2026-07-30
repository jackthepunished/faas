package reposcan

import (
	"errors"
	"fmt"
	"io/fs"
)

// readFirstValidFile returns the body and source filename of the
// first path in candidates that exists in fsys. Returns (nil, "",
// nil) when none of the candidates is present (a quiet skip — the
// tarball might not contain every format). Returns a non-nil error
// only when fs.ValidPath rejects a candidate, which never happens
// for hardcoded file lists but is the tripwire if a future tier-1
// detector hands in a derived path.
//
// Used by every Tier-1 detector to enumerate its format-specific
// filename set without each detector reinventing the loop.
func readFirstValidFile(fsys fs.FS, candidates []string) (body []byte, src string, err error) {
	for _, p := range candidates {
		if !fs.ValidPath(p) {
			return nil, "", fmt.Errorf("reposcan: invalid path %q", p)
		}
		b, err := fs.ReadFile(fsys, p)
		if err == nil {
			return b, p, nil
		}
		// Both ErrNotExist (file missing) and ErrInvalid (called on a
		// directory / non-regular file) classify as "next candidate".
		// Anything else bubbles up so we don't silently swallow an
		// I/O error that an operator needs to see.
		if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, fs.ErrInvalid) {
			return nil, "", err
		}
	}
	return nil, "", nil
}

// readValidFile reads a path from fsys, refusing any path that is not
// fs.ValidPath. fs.ValidPath rejects:
//   - empty strings
//   - paths containing a ".." path element
//   - paths starting with "/" (host-rooted)
//   - paths ending in "/"
//
// Two consequences matter:
//   - A tarball entry "subdir/../../../etc/passwd" fails closed: the
//     scanner never reads outside the archive root even if some
//     upstream extractor string-decatenates.
//   - A relative "compose.yaml" passes; a nested valid path like
//     "services/auth/Dockerfile" passes. These are the formats the
//     scanner is built to read.
//
// The MapFS path doesn't expose os.Open, so filesystem-level symlinks
// in a tarball extract don't reach the host — readValidFile rejects
// any fsys path on character alone. The fsys_safety_test pins both
// branches.
//
// All non-ValidPath outcomes are wrapped so callers can errors.Is-match
// against fmt.Errorf("%w: …"). Caller code may treat this as
// "expected, skip this source file" (a tarball with random sibling
// files is normal).
func readValidFile(fsys fs.FS, path string) ([]byte, error) {
	if !fs.ValidPath(path) {
		return nil, fmt.Errorf("reposcan: invalid path %q", path)
	}
	return fs.ReadFile(fsys, path)
}

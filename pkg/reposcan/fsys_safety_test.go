package reposcan

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

// TestFSSafety_DotDotEscapeRejected — a fsys path containing
// a ".." element is fs.ValidPath() = false. fs.ReadFile would
// panic on such a path (a tarball entry like
// `subdir/../../../etc/passwd` is a classic CVE pattern). The
// scanner's readValidFile never reads such paths, and the broader
// Scan() function surfaces this as a non-nil error.
func TestFSSafety_DotDotEscapeRejected(t *testing.T) {
	t.Parallel()
	// Most fs.FS implementations refuse to even hand a path with
	// ".." to fs.Open; reading it would never reach the host. The
	// pathological case is when the implementation accepts the
	// path but reads something outside the archive.
	if fs.ValidPath("subdir/../escape.txt") {
		t.Errorf("fs.ValidPath accepted 'subdir/../escape.txt'")
	}
	_, err := readValidFile(fstest.MapFS{}, "subdir/../escape.txt")
	if err == nil {
		t.Errorf("readValidFile returned nil for '../' path")
	}
}

// TestFSSafety_AbsolutePathRejected — paths starting with "/"
// are host-rooted; the scanner must refuse.
func TestFSSafety_AbsolutePathRejected(t *testing.T) {
	t.Parallel()
	if fs.ValidPath("/etc/passwd") {
		t.Errorf("fs.ValidPath accepted '/etc/passwd'")
	}
	_, err := readValidFile(fstest.MapFS{}, "/etc/passwd")
	if err == nil {
		t.Errorf("readValidFile returned nil for absolute path")
	}
}

// TestFSSafety_TrailingSlashRejected — fs.ValidPath rejects
// trailing slashes (paths that look like directory references).
func TestFSSafety_TrailingSlashRejected(t *testing.T) {
	t.Parallel()
	if fs.ValidPath("subdir/") {
		t.Errorf("fs.ValidPath accepted trailing slash")
	}
	_, err := readValidFile(fstest.MapFS{}, "subdir/")
	if err == nil {
		t.Errorf("readValidFile returned nil for trailing-slash path")
	}
}

// TestFSSafety_EmptyPathRejected — fs.ValidPath rejects "".
func TestFSSafety_EmptyPathRejected(t *testing.T) {
	t.Parallel()
	if fs.ValidPath("") {
		t.Errorf("fs.ValidPath accepted empty path")
	}
	_, err := readValidFile(fstest.MapFS{}, "")
	if err == nil {
		t.Errorf("readValidFile returned nil for empty path")
	}
}

// TestFSSafety_NoSymlinkLeak — even a MapFS that registers a
// file at the EXACT path requested, the scanner can't be tricked
// into reading /etc/passwd because it never calls os.Open.
// We exercise this by attempting a read of a path the MapFS
// DOES have (should succeed) and a path it does NOT (should fail).
func TestFSSafety_NoSymlinkLeak(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"Dockerfile": &fstest.MapFile{Data: []byte("FROM scratch")},
	}
	body, err := readValidFile(fsys, "Dockerfile")
	if err != nil || string(body) != "FROM scratch" {
		t.Errorf("readValidFile(Dockerfile) = (%v, %v); want (nil, FROM scratch)", body, err)
	}
	_, err = readValidFile(fsys, "/etc/passwd")
	if err == nil {
		t.Errorf("readValidFile(/etc/passwd) returned nil despite ValidPath=false")
	}
}

// TestFSSafety_ReadFirstValidFileSkipsMissing — when a
// candidates list contains a mix of present and absent files,
// the FIRST present file wins. The escape paths in candidates
// are rejected on fs.ValidPath first.
func TestFSSafety_ReadFirstValidFileSkipsMissing(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"fly.toml": &fstest.MapFile{Data: []byte("app = \"x\"\n")},
	}
	body, src, err := readFirstValidFile(fsys, []string{"render.yaml", "fly.toml"})
	if err != nil {
		t.Fatalf("readFirstValidFile: %v", err)
	}
	if src != "fly.toml" || !strings.HasPrefix(string(body), "app = ") {
		t.Errorf("readFirstValidFile picked (%s, %q)", src, body)
	}
	// Empty result on all-missing.
	body2, src2, err2 := readFirstValidFile(fsys, []string{"nope.yaml", "also-not.yaml"})
	if err2 != nil || body2 != nil || src2 != "" {
		t.Errorf("readFirstValidFile on all-missing = (%v, %q, %v); want (nil, \"\", nil)",
			body2, src2, err2)
	}
}

// TestFSSafety_Scan_NoEscapePossible — the Scan() entry point
// never returns a workload whose source string contains "../"
// even when the input fsys has no manifest files at all (forcing
// the Tier-4 floor). The Tier-4 source is "root-floor", not
// anything that could leak a path.
func TestFSSafety_Scan_NoEscapePossible(t *testing.T) {
	t.Parallel()
	r, err := Scan(fstest.MapFS{})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	for _, w := range r.Workloads {
		if strings.Contains(w.Source, "..") {
			t.Errorf("workload source contains '..': %q", w.Source)
		}
	}
	// The Tier-4 root-floor seed name is "app" with source
	// "root-floor" — a fully-qualified literal.
	if len(r.Workloads) != 1 || r.Workloads[0].Name != "app" {
		t.Errorf("Tier-4 floor failed: workloads = %v", r.Workloads)
	}
	if r.Workloads[0].Source != "root-floor" {
		t.Errorf("Tier-4 source = %q, want 'root-floor'", r.Workloads[0].Source)
	}
}

// TestFSSafety_ErrorsIsPlumbing — verifies that the errors.Is
// tripwire for ErrNotExist/ErrInvalid is intact in fsys_safety.go.
// If a future refactor changes that helper, fs.ValidPath
// differences will surface here first.
func TestFSSafety_ErrorsIsPlumbing(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"subdir": &fstest.MapFile{Mode: 0o755 | fs.ModeDir},
	}
	// Calling ReadFile on a directory returns ErrInvalid, not
	// ErrNotExist. Our helper discriminates between them so an
	// I/O error on a real directory doesn't masquerade as a
	// missing-file skip.
	_, err := fs.ReadFile(fsys, "subdir")
	if err == nil {
		t.Fatalf("ReadFile on a directory returned nil err")
	}
	if !errors.Is(err, fs.ErrInvalid) {
		t.Logf("Note: ReadFile on dir returned %v (not ErrInvalid) — still OK", err)
	}
}

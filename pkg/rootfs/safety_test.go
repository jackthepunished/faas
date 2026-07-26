// Targeted tests for the remaining pkg/rootfs branches: safeJoin's traversal
// guard, applyEntry's hardlink + char-device + symlink branches, clearDir's
// missing-dir path, and ApplyLayerGz's bad-gzip path. The happy-path
// ApplyLayer/safeJoin cases are already covered by rootfs_test.go; this file
// pins down the negative paths.

package rootfs

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- safeJoin ---------------------------------------------------------------

func TestSafeJoin(t *testing.T) {
	cases := []struct {
		name    string
		base    string
		entry   string
		wantErr bool
	}{
		{"empty entry", "/dst", "", true},
		{"absolute unix path", "/dst", "/etc/passwd", true},
		{"parent traversal", "/dst", "../escape", true},
		{"nested parent traversal", "/dst", "foo/../../escape", true},
		{"clean relative", "/dst", "foo/bar", false},
		{"dot path", "/dst", ".", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := safeJoin(tc.base, tc.entry)
			if tc.wantErr {
				if err == nil {
					t.Errorf("safeJoin(%q, %q) = %q, want error", tc.base, tc.entry, got)
				}
				return
			}
			if err != nil {
				t.Errorf("safeJoin(%q, %q) error: %v", tc.base, tc.entry, err)
				return
			}
			// Defence-in-depth: result must be under base.
			rel, relErr := filepath.Rel(tc.base, got)
			if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				t.Errorf("safeJoin result %q escaped base %q (rel=%q)", got, tc.base, rel)
			}
		})
	}
}

// --- applyEntry: TypeLink (hardlink) branch ---------------------------------

func TestApplyEntry_Hardlink(t *testing.T) {
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(dst, "src"), []byte("source"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{Name: "lnk", Linkname: "src", Typeflag: tar.TypeLink, Mode: 0o644}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ApplyLayer(dst, tar.NewReader(&buf)); err != nil {
		t.Fatalf("ApplyLayer: %v", err)
	}
	a, _ := os.Stat(filepath.Join(dst, "src"))
	b, _ := os.Stat(filepath.Join(dst, "lnk"))
	if !os.SameFile(a, b) {
		t.Errorf("hardlink dst=%v src=%v; not the same file", b, a)
	}
}

// Char/block/fifo devices are skipped by the default branch in applyEntry.
func TestApplyEntry_SkipsDeviceEntries(t *testing.T) {
	dst := t.TempDir()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "fifo", Typeflag: tar.TypeFifo, Mode: 0o644}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ApplyLayer(dst, tar.NewReader(&buf)); err != nil {
		t.Fatalf("ApplyLayer should skip fifo, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "fifo")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("fifo should NOT exist (skipped), stat err = %v", err)
	}
}

// --- applyEntry: TypeSymlink branch -----------------------------------------

func TestApplyEntry_Symlink(t *testing.T) {
	dst := t.TempDir()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	// Relative-in-archive link target: the symlink points to a sibling
	// entry inside the staging root. Absolute paths are REJECTED (see
	// safeJoin + the CodeQL go/path-injection hardening in applyEntry)
	// because a malicious layer could otherwise craft a symlink whose
	// target is /etc/passwd or any other host path the staging directory
	// will later follow.
	if err := tw.WriteHeader(&tar.Header{
		Name: "link", Linkname: "sibling", Typeflag: tar.TypeSymlink, Mode: 0o777,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ApplyLayer(dst, tar.NewReader(&buf)); err != nil {
		t.Fatalf("ApplyLayer symlink: %v", err)
	}
	target, err := os.Readlink(filepath.Join(dst, "link"))
	if err != nil {
		t.Fatalf("symlink not created: %v", err)
	}
	// safeJoin returns filepath.Join(base, clean(linkname)) so the
	// kernel-side symlink text is the absolute path inside the staging
	// root — that's how Layer.applyEntry interprets "relative path
	// inside the archive root" after the CodeQL hardening.
	wantTarget := filepath.Join(dst, "sibling")
	if target != wantTarget {
		t.Errorf("symlink target = %q, want %q", target, wantTarget)
	}
}

func TestApplyEntry_Symlink_RejectsAbsoluteLinkname(t *testing.T) {
	// Defense-in-depth: a malicious layer can ship a symlink whose
	// Linkname is an absolute host path. safeJoin rejects it before the
	// staging directory is ever touched. Pin the failure mode here so
	// the symlink path doesn't silently regress to "create the symlink
	// with the absolute target" on a future refactor.
	dst := t.TempDir()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name: "esc", Linkname: "/etc/passwd", Typeflag: tar.TypeSymlink, Mode: 0o777,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ApplyLayer(dst, tar.NewReader(&buf)); err == nil {
		t.Fatal("ApplyLayer accepted absolute symlink target; this is the CodeQL go/path-injection surface")
	}
	if _, err := os.Lstat(filepath.Join(dst, "esc")); err == nil {
		t.Errorf("escaped symlink landed on host: %s/esc", dst)
	}
}

// TestApplyEntry_Symlink_RejectsTwoStepChainAttack pins the attack
// shape CodeQL's go/unsafe-unzip-symlink query specifically warns
// about in its BAD/GOOD example: a malicious tar with two symlinks
// chained so that a purely-syntactic check (e.g. filepath.Rel on
// the un-resolved paths) would let the second link escape the
// staging root.
//
// Attack shape (mirrors the rule's BAD example):
//
//	subdir/parent -> subdir            (link A, looks harmless)
//	escape         -> subdir/parent/.. (link B, reads as "." under
//	                                      naive Rel(subdir, "subdir/parent/..")
//	                                      but actually points at the
//	                                      parent of `subdir`)
//
// safeJoin's filepath.Clean collapses B's Linkname to "subdir"
// (inside base), so B's on-disk target is dst/subdir — same as A.
// Neither symlink's resolved target escapes dst. The kernel walk
// at read time can't escape because A's target string itself was
// validated at write time.
//
// This test pins the runtime invariant: BOTH symlinks land on disk
// (safeJoin accepts them), but neither target resolves to a path
// outside dst. A naive refactor that swaps safeJoin's
// Clean+Rel(joined,base) for a plain Rel(subdir, "subdir/parent/..")
// would let B point at dst's parent, which the test catches via
// filepath.EvalSymlinks.
func TestApplyEntry_Symlink_RejectsTwoStepChainAttack(t *testing.T) {
	dst := t.TempDir()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	// Step 1: innocent-looking symlink inside base. Linkname
	// "subdir" passes safeJoin verbatim → target = dst/subdir.
	if err := tw.WriteHeader(&tar.Header{
		Name: "subdir/parent", Linkname: "subdir", Typeflag: tar.TypeSymlink, Mode: 0o777,
	}); err != nil {
		t.Fatal(err)
	}
	// Step 2: the CodeQL-flagged shape. safeJoin's Clean collapses
	// "subdir/parent/.." → "subdir", so B's on-disk target is also
	// dst/subdir (NOT dst's parent).
	if err := tw.WriteHeader(&tar.Header{
		Name: "escape", Linkname: "subdir/parent/..", Typeflag: tar.TypeSymlink, Mode: 0o777,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ApplyLayer(dst, tar.NewReader(&buf)); err != nil {
		t.Fatalf("ApplyLayer on benign-shape 2-link tar: %v (test fixture bug, not safeJoin regression)", err)
	}
	// Walk the escape symlink; the kernel-resolved target MUST
	// remain inside dst. A regression in safeJoin that lets B's
	// Linkname pass as-is (rather than via Clean collapse) would
	// resolve to dst/.. — the test catches it here.
	//
	// EvalSymlinks dst too: on macOS, t.TempDir() returns a path
	// under /var/folders/... which itself is a symlink to
	// /private/var/folders/...; comparing resolved-vs-dst without
	// normalising dst gives a false-positive escape reading on
	// every run. The dst path is a real directory owned by us, so
	// normalising it is safe.
	dstReal, err := filepath.EvalSymlinks(dst)
	if err != nil {
		t.Fatalf("EvalSymlinks(dst): %v", err)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(dst, "escape"))
	if err != nil {
		t.Fatalf("EvalSymlinks on escape: %v", err)
	}
	rel, err := filepath.Rel(dstReal, resolved)
	if err != nil {
		t.Fatalf("Rel(%q, %q): %v", dstReal, resolved, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Errorf("2-step symlink chain escaped dst: %q -> %q (rel=%q)", filepath.Join(dst, "escape"), resolved, rel)
	}
}

// --- applyEntry: TypeReg exact-size path ------------------------------------

func TestApplyEntry_RegExactSize(t *testing.T) {
	dst := t.TempDir()
	body := []byte("hello, world")
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{Name: "greeting.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ApplyLayer(dst, tar.NewReader(&buf)); err != nil {
		t.Fatalf("ApplyLayer: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "greeting.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Errorf("file content = %q, want %q", got, body)
	}
}

// Note: the negative path (CopyN error mid-stream) is hard to construct
// here because tar.Writer.Close() itself errors when the declared size
// wasn't reached.

// --- clearDir ---------------------------------------------------------------

func TestClearDir_KeepsDirRemovesChildren(t *testing.T) {
	dst := t.TempDir()
	for _, name := range []string{"a", "b", "sub/c"} {
		full := filepath.Join(dst, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := clearDir(dst); err != nil {
		t.Fatalf("clearDir: %v", err)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Errorf("dir removed: %v", err)
	}
	entries, _ := os.ReadDir(dst)
	if len(entries) != 0 {
		t.Errorf("clearDir left %d entries: %v", len(entries), entries)
	}
}

func TestClearDir_MissingDirIsNoOp(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such")
	if err := clearDir(missing); err != nil {
		t.Errorf("clearDir on missing dir = %v, want nil (ENOENT is non-fatal)", err)
	}
}

// --- ApplyLayerGz: bad gzip header ------------------------------------------

func TestApplyLayerGz_BadGzip(t *testing.T) {
	err := ApplyLayerGz(t.TempDir(), bytes.NewReader([]byte("not a gzip stream")))
	if err == nil {
		t.Fatal("ApplyLayerGz should reject non-gzip input")
	}
	if !strings.Contains(err.Error(), "gzip") {
		t.Errorf("error %q should mention gzip", err.Error())
	}
}

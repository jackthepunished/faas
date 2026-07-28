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
	// A symlink's Linkname is stored VERBATIM: it is guest-side data that
	// the guest kernel resolves once the ext4 is mounted at "/". A
	// relative target must stay relative.
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
	// Verbatim — NOT filepath.Join(dst, "sibling"). Rewriting the target
	// to a host staging path bakes /tmp/faas-base-XXXX into the image and
	// produces a rootfs whose symlinks all dangle inside the guest.
	if target != "sibling" {
		t.Errorf("symlink target = %q, want %q (verbatim)", target, "sibling")
	}
}

// TestApplyEntry_Symlink_AbsoluteTargetStoredVerbatim is the regression pin
// for the imaged crash-loop that took cd-digitalocean red on every merge to
// main (imaged: "rootfs: absolute entry path \"/bin/busybox\" rejected").
//
// Commit 7805f76 routed symlink Linknames through safeJoin to silence a
// CodeQL go/path-injection alert. safeJoin rejects absolute paths — but
// absolute symlink targets are the NORM in OCI images, not an attack: the
// alpine base layer alone ships 306 of them (bin/arch, bin/ash, bin/cat …
// all -> /bin/busybox), and every Debian/Ubuntu image does the same. The
// result was that imaged could not build a base rootfs from any real image
// and crash-looped at startup.
//
// The target must be stored exactly as declared. Safety comes from the
// host never FOLLOWING it — see TestApplyLayer_WriteThroughSymlinkIsClamped.
func TestApplyEntry_Symlink_AbsoluteTargetStoredVerbatim(t *testing.T) {
	dst := t.TempDir()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	// Exactly the alpine shape.
	if err := tw.WriteHeader(&tar.Header{
		Name: "bin/busybox", Typeflag: tar.TypeReg, Mode: 0o755, Size: 0,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: "bin/sh", Linkname: "/bin/busybox", Typeflag: tar.TypeSymlink, Mode: 0o777,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ApplyLayer(dst, tar.NewReader(&buf)); err != nil {
		t.Fatalf("ApplyLayer rejected an absolute symlink target: %v\n"+
			"this is the imaged crash-loop regression (commit 7805f76)", err)
	}
	got, err := os.Readlink(filepath.Join(dst, "bin/sh"))
	if err != nil {
		t.Fatalf("bin/sh symlink not created: %v", err)
	}
	if got != "/bin/busybox" {
		t.Errorf("bin/sh -> %q, want %q (verbatim, guest-side path)", got, "/bin/busybox")
	}
}

// TestApplyLayer_WriteThroughSymlinkIsClamped pins the security property
// that makes verbatim Linknames safe, and is the real answer to the CodeQL
// go/path-injection alert that 7805f76 mis-fixed.
//
// A hostile layer ships "escape -> /etc" and then writes "escape/passwd".
// The link text is stored as-is, but the host resolves the later write
// with resolveWithin, which re-anchors absolute link targets at the
// staging root. The write must land inside dst and the host's real
// /etc/passwd must be untouched.
func TestApplyLayer_WriteThroughSymlinkIsClamped(t *testing.T) {
	dst := t.TempDir()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name: "escape", Linkname: "/etc", Typeflag: tar.TypeSymlink, Mode: 0o777,
	}); err != nil {
		t.Fatal(err)
	}
	body := []byte("PWNED")
	if err := tw.WriteHeader(&tar.Header{
		Name: "escape/passwd", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)),
	}); err != nil {
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
	// The write landed inside staging, at <dst>/etc/passwd.
	clamped := filepath.Join(dst, "etc", "passwd")
	got, err := os.ReadFile(clamped)
	if err != nil {
		t.Fatalf("clamped write not found at %s: %v", clamped, err)
	}
	if string(got) != string(body) {
		t.Errorf("clamped content = %q, want %q", got, body)
	}
	// And the link text itself is still the verbatim guest-side value.
	link, err := os.Readlink(filepath.Join(dst, "escape"))
	if err != nil {
		t.Fatal(err)
	}
	if link != "/etc" {
		t.Errorf("link text = %q, want %q", link, "/etc")
	}
}

// TestResolveWithin_SymlinkCycleTerminates pins the SYMLOOP_MAX bound: a
// layer with a symlink cycle must fail the build, not hang the daemon.
func TestResolveWithin_SymlinkCycleTerminates(t *testing.T) {
	dst := t.TempDir()
	if err := os.Symlink("b", filepath.Join(dst, "a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a", filepath.Join(dst, "b")); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveWithin(dst, "a/file"); err == nil {
		t.Fatal("expected a symlink-cycle error, got nil")
	}
}

// TestResolveWithin_MergedUsrTraversal pins the Debian-style layout that a
// reject-on-ancestor-symlink guard would have broken: "bin -> usr/bin" in
// one layer, "bin/sh" written in a later one.
func TestResolveWithin_MergedUsrTraversal(t *testing.T) {
	dst := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dst, "usr", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("usr/bin", filepath.Join(dst, "bin")); err != nil {
		t.Fatal(err)
	}
	got, err := resolveEntryPath(dst, "bin/sh")
	if err != nil {
		t.Fatalf("merged-usr traversal rejected: %v", err)
	}
	want := filepath.Join(dst, "usr", "bin", "sh")
	if got != want {
		t.Errorf("resolveEntryPath = %q, want %q", got, want)
	}
}

// TestApplyEntry_Symlink_TwoStepChainCannotEscapeWrites pins the attack
// shape CodeQL's go/unsafe-unzip-symlink query warns about: a malicious tar
// chaining two symlinks so a purely-syntactic check would let a later WRITE
// escape the staging root.
//
// Attack shape:
//
//	subdir/parent -> ..                (link A: from inside subdir, "up")
//	escape        -> subdir/parent/..  (link B: two levels up = dst's parent)
//	escape/pwned                       (the actual write attempt)
//
// Both links are stored verbatim — they are guest-side strings and harmless
// on their own. The security boundary is the WRITE: resolveWithin consumes
// ".." with a clamp at root, so B cannot walk above dst and the payload
// lands inside staging. Nothing is created next to dst on the host.
func TestApplyEntry_Symlink_TwoStepChainCannotEscapeWrites(t *testing.T) {
	parentDir := t.TempDir()
	dst := filepath.Join(parentDir, "staging")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	// Canary next to dst: the attack's real goal is to clobber this.
	canary := filepath.Join(parentDir, "pwned")

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name: "subdir/parent", Linkname: "..", Typeflag: tar.TypeSymlink, Mode: 0o777,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: "escape", Linkname: "subdir/parent/..", Typeflag: tar.TypeSymlink, Mode: 0o777,
	}); err != nil {
		t.Fatal(err)
	}
	body := []byte("PWNED")
	if err := tw.WriteHeader(&tar.Header{
		Name: "escape/pwned", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ApplyLayer(dst, tar.NewReader(&buf)); err != nil {
		t.Fatalf("ApplyLayer on 2-link tar: %v", err)
	}

	// The canary must NOT exist: the write never escaped dst.
	if _, err := os.Lstat(canary); err == nil {
		t.Fatalf("2-step symlink chain escaped staging and wrote %s", canary)
	}
	// Nothing at all should have been created outside dst.
	entries, err := os.ReadDir(parentDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "staging" {
			t.Errorf("unexpected host-side entry created outside staging: %q", e.Name())
		}
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

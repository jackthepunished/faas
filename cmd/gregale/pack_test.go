package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	crypto_rand "crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeFile is a tiny helper: create parent dirs + write content.
func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", p, err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}

// writeFileBytes writes raw bytes to a path (helper for size-cap tests that
// need to materialise files of arbitrary size).
func writeFileBytes(t *testing.T, p string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", p, err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}

// tarEntries reads a gzipped tarball and returns the set of entry names
// (slash-separated, directories keep their trailing slash). Uses os.ReadFile
// (not os.Open) to stay clear of the cmd/gregale forbidigo rule.
func tarEntries(t *testing.T, path string) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read tarball: %v", err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	out := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		out[hdr.Name] = true
	}
	return out
}

func TestDetectFramework(t *testing.T) {
	cases := []struct {
		name  string
		files []string // relative paths to seed (empty file each)
		want  framework
	}{
		{"node", []string{"package.json", "index.js"}, fwNode},
		{"python_requirements", []string{"requirements.txt"}, fwPython},
		{"python_pyproject", []string{"pyproject.toml"}, fwPython},
		{"python_pipfile", []string{"Pipfile"}, fwPython},
		{"python_setup", []string{"setup.py"}, fwPython},
		{"go", []string{"go.mod", "main.go"}, fwGo},
		{"dockerfile_wins_over_node", []string{"Dockerfile", "package.json"}, fwDocker},
		{"dockerfile_case_insensitive", []string{"dockerfile"}, fwDocker},
		{"empty", nil, fwUnknown},
		{"unrelated_only", []string{"README.md", "notes.txt"}, fwUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range tc.files {
				writeFile(t, dir, f, "")
			}
			if got := detectFramework(dir); got != tc.want {
				t.Errorf("detectFramework = %q, want %q", got, tc.want)
			}
		})
	}
}

// A go.mod buried in a subdirectory must NOT be detected — the rule is
// top-level-only, matching pkg/builderd/detect.go.
func TestDetectFramework_NestedMarkerIgnored(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "svc/go.mod", "module x")
	writeFile(t, dir, "README.md", "hi")
	if got := detectFramework(dir); got != fwUnknown {
		t.Errorf("detectFramework = %q, want %q (nested go.mod must not count)", got, fwUnknown)
	}
}

func TestPackDirToTarGz_TopLevelDirAndCount(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Base(dir)
	writeFile(t, dir, "package.json", "{}")
	writeFile(t, dir, "src/index.js", "console.log(1)")

	dest := filepath.Join(t.TempDir(), "out.tar.gz")
	n, err := packDirToTarGz(dir, dest)
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	if n != 2 {
		t.Errorf("fileCount = %d, want 2", n)
	}
	got := tarEntries(t, dest)
	for _, want := range []string{base + "/package.json", base + "/src/index.js"} {
		if !got[want] {
			t.Errorf("archive missing %q; entries: %v", want, got)
		}
	}
}

func TestPackDirToTarGz_Excludes(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Base(dir)
	// Kept:
	writeFile(t, dir, "package.json", "{}")
	writeFile(t, dir, ".env", "SECRET=1") // dotfile kept on purpose
	writeFile(t, dir, ".dockerignore", "node_modules")
	// Dropped:
	writeFile(t, dir, ".git/HEAD", "ref: refs/heads/main")
	writeFile(t, dir, "node_modules/left-pad/index.js", "module.exports=1")
	writeFile(t, dir, "vendor/x/x.go", "package x")
	writeFile(t, dir, "app/__pycache__/mod.cpython-312.pyc", "bytecode")
	writeFile(t, dir, "app/mod.pyc", "bytecode")
	writeFile(t, dir, ".DS_Store", "junk")

	dest := filepath.Join(t.TempDir(), "out.tar.gz")
	n, err := packDirToTarGz(dir, dest)
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	got := tarEntries(t, dest)

	mustHave := []string{base + "/package.json", base + "/.env", base + "/.dockerignore"}
	for _, w := range mustHave {
		if !got[w] {
			t.Errorf("archive should contain %q; entries: %v", w, got)
		}
	}
	// n counts regular files: package.json, .env, .dockerignore = 3.
	if n != 3 {
		t.Errorf("fileCount = %d, want 3 (only kept files); entries: %v", n, got)
	}
	for name := range got {
		for _, bad := range []string{"/.git/", "/node_modules/", "/vendor/", "/__pycache__/", ".pyc", ".DS_Store"} {
			if strings.Contains(name, bad) {
				t.Errorf("archive should not contain %q (matched %q)", name, bad)
			}
		}
	}
}

func TestPackDirToTarGz_RejectsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.Symlink not supported on Windows")
	}
	dir := t.TempDir()
	writeFile(t, dir, "real.txt", "hi")
	if err := os.Symlink(filepath.Join(dir, "real.txt"), filepath.Join(dir, "link.txt")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "out.tar.gz")
	if _, err := packDirToTarGz(dir, dest); err == nil {
		t.Fatal("packDirToTarGz should reject a symlink, got nil error")
	}
}

func TestPackDirToTarGz_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(t.TempDir(), "out.tar.gz")
	n, err := packDirToTarGz(dir, dest)
	if err != nil {
		t.Fatalf("pack empty: %v", err)
	}
	if n != 0 {
		t.Errorf("fileCount = %d, want 0", n)
	}
	// Archive must still be a valid, readable gzip tar (possibly zero entries).
	_ = tarEntries(t, dest)
}

// TestPackDirToTarGz_TotalSizeCap pins the zero-config zeroConfigSourceCapMB
// preflight: a cwd that packs to > cap must surface a friendly error before
// any HTTP round-trip. Uses many small files so per-file is not the trigger.
// Bytes are crypto/random so gzip can't compress them away.
func TestPackDirToTarGz_TotalSizeCap(t *testing.T) {
	if testing.Short() {
		t.Skip("size-cap test materialises > 100 MB; skip in -short mode")
	}
	dir := t.TempDir()
	const oneMiB = 1024 * 1024
	// 110 × 1 MiB of crypto-random bytes (110 MiB raw, ~110 MiB after gzip).
	// Each file is well under the per-file cap (100 MiB), so the total-cap
	// stat check is what trips.
	const totalFiles = zeroConfigSourceCapMB + 10
	for i := 0; i < totalFiles; i++ {
		chunk := make([]byte, oneMiB)
		if _, err := io.ReadFull(crypto_rand.Reader, chunk); err != nil {
			t.Fatalf("rand: %v", err)
		}
		writeFileBytes(t, filepath.Join(dir, fmt.Sprintf("chunk-%04d.bin", i)), chunk)
	}

	dest := filepath.Join(t.TempDir(), "out.tar.gz")
	_, err := packDirToTarGz(dir, dest)
	if err == nil {
		t.Fatal("packDirToTarGz should reject total size > cap, got nil error")
	}
	if !strings.Contains(err.Error(), "zero-config cap") {
		t.Errorf("expected friendly total-cap error; got %v", err)
	}
}

// TestPackDirToTarGz_PerFileCap pins the per-file guard inside copyRegular: a
// single file >= cap must be rejected while still being streamed, instead of
// materialising the whole thing into the gzip writer first.
func TestPackDirToTarGz_PerFileCap(t *testing.T) {
	if testing.Short() {
		t.Skip("per-file cap test materialises a > cap file")
	}
	dir := t.TempDir()
	huge := make([]byte, zeroConfigSourceCapMB*1024*1024)
	if _, err := io.ReadFull(crypto_rand.Reader, huge); err != nil {
		t.Fatalf("rand: %v", err)
	}
	writeFileBytes(t, filepath.Join(dir, "blob.bin"), huge)

	dest := filepath.Join(t.TempDir(), "out.tar.gz")
	_, err := packDirToTarGz(dir, dest)
	if err == nil {
		t.Fatal("packDirToTarGz should reject a single file >= per-file cap, got nil")
	}
	if !strings.Contains(err.Error(), "per-file cap") {
		t.Errorf("expected per-file cap error; got %v", err)
	}
}

// TestPackDirToTarGz_JustUnderTotalCap passes when the packed tarball sits
// predictably under cap — pins that the preflight doesn't false-positive.
// ~50 MiB raw (compressible to a fraction under cap).
func TestPackDirToTarGz_JustUnderTotalCap(t *testing.T) {
	if testing.Short() {
		t.Skip("size-cap test materialises near-cap; skip in -short mode")
	}
	dir := t.TempDir()
	// 50 MiB raw, written as one file under the per-file cap. Far enough
	// from zeroConfigSourceCapMB (100 MB) that gzip compression won't
	// matter — even if it expands slightly the tarball stays well under
	// cap.
	const fiftyMiB = 50 * 1024 * 1024
	chunk := make([]byte, fiftyMiB)
	if _, err := io.ReadFull(crypto_rand.Reader, chunk); err != nil {
		t.Fatalf("rand: %v", err)
	}
	writeFileBytes(t, filepath.Join(dir, "well_under.bin"), chunk)

	dest := filepath.Join(t.TempDir(), "out.tar.gz")
	if _, err := packDirToTarGz(dir, dest); err != nil {
		t.Fatalf("packDirToTarGz well under cap, want pass; got %v", err)
	}
}

func TestAutoPackCwd(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "package.json", "{}")
	writeFile(t, dir, "index.js", "x")

	path, fw, n, err := autoPackCwd(dir)
	if err != nil {
		t.Fatalf("autoPackCwd: %v", err)
	}
	defer func() { _ = os.Remove(path) }()
	if fw != fwNode {
		t.Errorf("framework = %q, want %q", fw, fwNode)
	}
	if n != 2 {
		t.Errorf("fileCount = %d, want 2", n)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected tarball at %s: %v", path, err)
	}
	_ = tarEntries(t, path) // must be a valid gzipped tar
}

// On a pack error autoPackCwd must not leave its temp file behind.
func TestAutoPackCwd_CleansUpOnError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.Symlink not supported on Windows")
	}
	dir := t.TempDir()
	writeFile(t, dir, "real.txt", "hi")
	if err := os.Symlink(filepath.Join(dir, "real.txt"), filepath.Join(dir, "link.txt")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	path, _, _, err := autoPackCwd(dir)
	if err == nil {
		t.Fatal("expected error from autoPackCwd on symlink dir")
	}
	if path != "" {
		t.Errorf("path = %q, want empty on error", path)
	}
}

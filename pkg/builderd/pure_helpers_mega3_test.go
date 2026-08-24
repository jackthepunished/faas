// pure_helpers_mega3_test.go — Coverage Mega-PR #3 cluster D:
// fill pkg/builderd coverage of the small pure or thin-wrapper
// helpers beyond what cluster-0 (PR #1074) already covered.
//
// Targets (baseline 70.2% on the package at branch time):
//   - defaultCacheDir (0%): honour Config.CacheDir / fall back to
//     /var/cache/faas/builds canonical path.
//   - Detector.DetectFromFS (0%): thin pass-through to
//     pkg/markers.DetectFromFS.
//   - dirSize (84.6%): best-effort sum of regular-file sizes.
//   - hashFile (88.9%): SHA-256 hex of file contents.
//   - NewVMMDriver (0%): non-metal stub constructor.
//
// Whitebox `package builderd`.

package builderd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func TestDefaultCacheDir_HonoursConfig_Mega3(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{name: "empty falls back to canonical path", cfg: Config{}, want: "/var/cache/faas/builds"},
		{name: "operator override wins", cfg: Config{CacheDir: "/srv/faas/builds"}, want: "/srv/faas/builds"},
		{name: "tmp dir override", cfg: Config{CacheDir: "/tmp/builderd-cache"}, want: "/tmp/builderd-cache"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := defaultCacheDir(c.cfg)
			if got != c.want {
				t.Errorf("defaultCacheDir(%+v) = %q, want %q", c.cfg, got, c.want)
			}
		})
	}
}

func TestDetectFromFS_DelegatesToMarkers_Mega3(t *testing.T) {
	t.Parallel()
	d := NewDetector()

	cases := []struct {
		name string
		fsys fstest.MapFS
		want Framework
	}{
		{
			name: "package.json → node",
			fsys: fstest.MapFS{"package.json": &fstest.MapFile{Data: []byte(`{}`)}},
			want: FrameworkNode,
		},
		{
			name: "requirements.txt → python",
			fsys: fstest.MapFS{"requirements.txt": &fstest.MapFile{Data: []byte("flask")}},
			want: FrameworkPython,
		},
		{
			name: "go.mod → go",
			fsys: fstest.MapFS{"go.mod": &fstest.MapFile{Data: []byte("module x")}},
			want: FrameworkGo,
		},
		{
			name: "Dockerfile → docker (priority over go.mod)",
			fsys: fstest.MapFS{
				"Dockerfile": &fstest.MapFile{Data: []byte("FROM scratch")},
				"go.mod":     &fstest.MapFile{Data: []byte("module x")},
			},
			want: FrameworkDocker,
		},
		{
			name: "no markers → unknown (no error)",
			fsys: fstest.MapFS{"README.md": &fstest.MapFile{Data: []byte("# readme")}},
			want: FrameworkUnknown,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := d.DetectFromFS(c.fsys)
			if err != nil {
				t.Fatalf("DetectFromFS: %v", err)
			}
			if got != c.want {
				t.Errorf("DetectFromFS = %q, want %q", got, c.want)
			}
		})
	}
}

func TestDirSize_SumsRegularFiles_Mega3(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mustWriteFileMega3(t, filepath.Join(root, "a"), fillerMega3(100))
	mustWriteFileMega3(t, filepath.Join(root, "b"), fillerMega3(250))
	mustWriteFileMega3(t, filepath.Join(root, "sub", "c"), fillerMega3(75))

	got, err := dirSize(root)
	if err != nil {
		t.Fatalf("dirSize: %v", err)
	}
	want := int64(100 + 250 + 75)
	if got != want {
		t.Errorf("dirSize = %d, want %d", got, want)
	}
}

func TestDirSize_EmptyRoot_Mega3(t *testing.T) {
	t.Parallel()
	got, err := dirSize(t.TempDir())
	if err != nil {
		t.Fatalf("dirSize(empty): %v", err)
	}
	if got != 0 {
		t.Errorf("dirSize(empty) = %d, want 0", got)
	}
}

func TestHashFile_SHA256OfContents_Mega3(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "blob")
	mustWriteFileMega3(t, path, []byte("hello world\n"))

	got, err := hashFile(path)
	if err != nil {
		t.Fatalf("hashFile: %v", err)
	}
	const want = "a948904f2f0f479b8f8197694b30184b0d2ed1c1cd2a1ec0fb85d299a192a447"
	if got != want {
		t.Errorf("hashFile = %q, want %q", got, want)
	}
}

func TestHashFile_MissingFile_Mega3(t *testing.T) {
	t.Parallel()
	_, err := hashFile(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("hashFile(missing): want error, got nil")
	}
	if !strings.Contains(err.Error(), "hash: open") {
		t.Errorf("hashFile(missing) error = %q, want wrapped 'hash: open'", err)
	}
}

func TestNewVMMDriver_StubReturnsZeroValue_Mega3(t *testing.T) {
	t.Parallel()
	drv, err := NewVMMDriver("ignored", "ignored", "ignored", "ignored")
	if err != nil {
		t.Fatalf("NewVMMDriver: %v", err)
	}
	if drv == nil {
		t.Fatal("NewVMMDriver: got nil driver, want non-nil zero value")
	}
}

// --- helpers --------------------------------------------------------

func mustWriteFileMega3(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func fillerMega3(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'A'
	}
	return b
}

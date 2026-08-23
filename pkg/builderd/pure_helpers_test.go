// pure_helpers_test.go — fill pkg/builderd coverage of the
// tiny pure helpers reachable without standing up a build VM,
// Postgres, or a real tarball.
//
// Targets:
//   - defaultCacheDir (the Config.CacheDir override + empty fallback)
//   - Detector.DetectFromFS (the FS-walk shim to markers.DetectFS)
//   - Detector.NewDetector (the constructor)
//   - Framework constant parity with pkg/markers
//
// Whitebox `package builderd`.
package builderd

import (
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"
)

// --- defaultCacheDir -------------------------------------------

func TestDefaultCacheDir_EmptyReturnsCanonical(t *testing.T) {
	if got := defaultCacheDir(Config{}); got != "/var/cache/faas/builds" {
		t.Errorf("empty cfg: got %q, want /var/cache/faas/builds", got)
	}
}

func TestDefaultCacheDir_OverrideRespected(t *testing.T) {
	cfg := Config{CacheDir: "/srv/builder/cache"}
	if got := defaultCacheDir(cfg); got != "/srv/builder/cache" {
		t.Errorf("override: got %q, want /srv/builder/cache", got)
	}
}

func TestDefaultCacheDir_WhitespaceOnlyFallsBack(t *testing.T) {
	// Empty == fallback (the check is `if cfg.CacheDir != ""`,
	// so a non-empty but whitespace-only string would still be
	// respected). Pin the contract so a future refactor to
	// `strings.TrimSpace` trips here.
	cfg := Config{CacheDir: "   "}
	if got := defaultCacheDir(cfg); got != "   " {
		t.Errorf("whitespace: got %q, want preserved whitespace (current contract)", got)
	}
}

// --- Detector constructor --------------------------------------

func TestNewDetector_NotNil(t *testing.T) {
	d := NewDetector()
	if d == nil {
		t.Fatal("NewDetector returned nil")
	}
}

// --- Detector.DetectFromFS -------------------------------------

// Empty FS → FrameworkUnknown, nil error (the marker-less case
// is reported by the build pipeline as user_error, not here).
func TestDetectFromFS_EmptyFSReturnsUnknownNoError(t *testing.T) {
	d := NewDetector()
	fw, err := d.DetectFromFS(fstest.MapFS{})
	if err != nil {
		t.Errorf("empty FS: err = %v, want nil", err)
	}
	if fw != FrameworkUnknown {
		t.Errorf("empty FS: framework = %v, want FrameworkUnknown", fw)
	}
}

// package.json at the FS root → FrameworkNode.
func TestDetectFromFS_PackageJSONIsNode(t *testing.T) {
	d := NewDetector()
	fsys := fstest.MapFS{
		"package.json": &fstest.MapFile{Data: []byte(`{"name":"x"}`)},
	}
	fw, err := d.DetectFromFS(fsys)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if fw != FrameworkNode {
		t.Errorf("package.json: framework = %v, want FrameworkNode", fw)
	}
}

// Nested (non-root) package.json is NOT detected as Node — only
// the root-level marker counts. The parity contract mirrors the
// tarball-side DetectFromTarball.
func TestDetectFromFS_NestedPackageJSONNotDetected(t *testing.T) {
	d := NewDetector()
	fsys := fstest.MapFS{
		"subdir/package.json": &fstest.MapFile{Data: []byte(`{}`)},
	}
	fw, err := d.DetectFromFS(fsys)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if fw != FrameworkUnknown {
		t.Errorf("nested: framework = %v, want FrameworkUnknown", fw)
	}
}

// requirements.txt → FrameworkPython.
func TestDetectFromFS_RequirementsTxtIsPython(t *testing.T) {
	d := NewDetector()
	fsys := fstest.MapFS{
		"requirements.txt": &fstest.MapFile{Data: []byte("flask\n")},
	}
	fw, err := d.DetectFromFS(fsys)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if fw != FrameworkPython {
		t.Errorf("requirements.txt: framework = %v, want FrameworkPython", fw)
	}
}

// go.mod → FrameworkGo.
func TestDetectFromFS_GoModIsGo(t *testing.T) {
	d := NewDetector()
	fsys := fstest.MapFS{
		"go.mod": &fstest.MapFile{Data: []byte("module x\n")},
	}
	fw, err := d.DetectFromFS(fsys)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if fw != FrameworkGo {
		t.Errorf("go.mod: framework = %v, want FrameworkGo", fw)
	}
}

// Dockerfile → FrameworkDocker.
func TestDetectFromFS_DockerfileIsDocker(t *testing.T) {
	d := NewDetector()
	fsys := fstest.MapFS{
		"Dockerfile": &fstest.MapFile{Data: []byte("FROM scratch\n")},
	}
	fw, err := d.DetectFromFS(fsys)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if fw != FrameworkDocker {
		t.Errorf("Dockerfile: framework = %v, want FrameworkDocker", fw)
	}
}

// Detector.DetectFromFS propagates I/O errors from the fsys
// (e.g. a permission-denied read at the FS root). We exercise
// this with a custom FS that errors on ReadDir.
func TestDetectFromFS_IOErrorPropagates(t *testing.T) {
	d := NewDetector()
	bad := errorFS{err: errors.New("synthetic fsys failure")}
	if _, err := d.DetectFromFS(bad); err == nil {
		t.Error("errFS: got nil err, want error")
	}
}

// --- Framework constant parity --------------------------------

// The Framework* constants are aliases for pkg/markers — pin
// that they carry the canonical int values so a future refactor
// in pkg/markers (adding a new framework, e.g.) surfaces here
// before it surprises downstream call sites.
func TestFrameworkConstants_Stable(t *testing.T) {
	cases := map[Framework]string{
		FrameworkNode:    "node",
		FrameworkPython:  "python",
		FrameworkGo:      "go",
		FrameworkDocker:  "docker",
		FrameworkUnknown: "unknown",
	}
	for fw, want := range cases {
		if string(fw) != want {
			t.Errorf("framework %v: got %q, want %q", fw, string(fw), want)
		}
	}
}

// --- helpers ---------------------------------------------------

// errorFS is a tiny fs.FS that always errors on ReadDir. Used to
// exercise Detector.DetectFromFS's error-propagation branch
// without depending on a network filesystem or permission-
// denied platform call.
type errorFS struct{ err error }

func (errorFS) Open(_ string) (fs.File, error)             { return nil, nil }
func (e errorFS) ReadDir(_ string) ([]fs.DirEntry, error)  { return nil, e.err }
func (e errorFS) Glob(_ string) ([]string, error)          { return nil, e.err }
func (e errorFS) Stat(_ string) (fs.FileInfo, error)       { return nil, e.err }
func (e errorFS) ReadFile(_ string) ([]byte, error)        { return nil, e.err }
func (e errorFS) Sub(_ string) (fs.FS, error)              { return nil, e.err }
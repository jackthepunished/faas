package builderd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

// makeTarball produces a tarball at path whose root contains the given
// filenames (used to seed detector fixtures). Empty content is fine.
func makeTarball(t *testing.T, path string, names []string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, n := range names {
		hdr := &tar.Header{Name: n, Mode: 0o644, Size: 0, Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDetector_Node(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "src.tar.gz")
	makeTarball(t, path, []string{"package.json", "index.js", "lib/util.js"})

	d := NewDetector()
	got, err := d.Detect(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != FrameworkNode {
		t.Errorf("framework = %s, want node", got)
	}
}

func TestDetector_Python(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "src.tar.gz")
	makeTarball(t, path, []string{"requirements.txt", "main.py"})

	d := NewDetector()
	got, err := d.Detect(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != FrameworkPython {
		t.Errorf("framework = %s, want python", got)
	}
}

func TestDetector_DockerfileWins(t *testing.T) {
	// A Dockerfile at the root wins over package.json — matches the user
	// experience of `faas deploy --dockerfile`.
	dir := t.TempDir()
	path := filepath.Join(dir, "src.tar.gz")
	makeTarball(t, path, []string{"Dockerfile", "package.json"})

	d := NewDetector()
	got, err := d.Detect(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != FrameworkDocker {
		t.Errorf("framework = %s, want docker", got)
	}
}

func TestDetector_Go(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "src.tar.gz")
	makeTarball(t, path, []string{"go.mod", "main.go", "internal/server.go"})

	d := NewDetector()
	got, err := d.Detect(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != FrameworkGo {
		t.Errorf("framework = %s, want go", got)
	}
}

func TestDetector_DockerfileBeatsGo(t *testing.T) {
	// A root Dockerfile wins over go.mod — mirrors the user expectation
	// of `faas deploy --dockerfile` taking precedence over a coincidental
	// go.mod that lives in a Go project that ALSO ships a Dockerfile.
	dir := t.TempDir()
	path := filepath.Join(dir, "src.tar.gz")
	makeTarball(t, path, []string{"Dockerfile", "go.mod", "main.go"})

	d := NewDetector()
	got, err := d.Detect(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != FrameworkDocker {
		t.Errorf("framework = %s, want docker (Dockerfile wins over go.mod)", got)
	}
}

func TestDetector_PythonBeatsGo(t *testing.T) {
	// Defensive priority pin: a root go.mod alongside a coincidental
	// requirements.txt must resolve to python, not go. The two markers
	// should not co-occur in practice, but the priority must be explicit
	// so a future re-ordering of the switch arms doesn't silently change
	// the chosen build engine. The order is docker > node > python > go
	// (python is checked before go in the detector's switch), so python
	// wins; this is intentional because a requirements.txt alongside a
	// go.mod most likely indicates a polyglot project where the Python
	// side is the primary deploy target. (If the order ever needs to
	// flip, this test name and its expected value are the place to
	// change.)
	dir := t.TempDir()
	path := filepath.Join(dir, "src.tar.gz")
	makeTarball(t, path, []string{"go.mod", "requirements.txt", "main.go", "app.py"})

	d := NewDetector()
	got, err := d.Detect(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != FrameworkPython {
		t.Errorf("framework = %s, want python (priority pin: python wins over go when both markers are root-level)", got)
	}
}

func TestDetector_Unknown(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "src.tar.gz")
	makeTarball(t, path, []string{"README.md", "src/main.c"})

	d := NewDetector()
	if _, err := d.Detect(path); err == nil {
		t.Error("expected error on unrecognized source")
	}
}

func TestDetector_NestedEntriesIgnored(t *testing.T) {
	// package.json buried in a subdir is NOT a project-level package.json.
	dir := t.TempDir()
	path := filepath.Join(dir, "src.tar.gz")
	makeTarball(t, path, []string{"subdir/package.json", "requirements.txt"})

	d := NewDetector()
	got, err := d.Detect(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != FrameworkPython {
		t.Errorf("framework = %s, want python (top-level wins)", got)
	}
}

func TestDetector_BadTarball(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.tar.gz")
	if err := os.WriteFile(path, []byte("not a tarball"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDetector().Detect(path); err == nil {
		t.Error("expected error on malformed tarball")
	}
}

func TestDetector_MissingFile(t *testing.T) {
	if _, err := NewDetector().Detect("/no/such/file.tar.gz"); err == nil {
		t.Error("expected error on missing file")
	}
}

package markers_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/onebox-faas/faas/pkg/markers"
)

// seedBoth creates a t.TempDir() with the given files (forward-
// slash names, empty content), then returns BOTH an os.DirFS
// view (CLI-style) and a gzipped tarball of the same files
// (server-style). Both views represent the same source.
//
// The tarball preserves the full path of each file (so
// `apps/web/package.json` lands at `apps/web/package.json` in
// the archive), mirroring what apid produces when packing
// source for an upload. This is what makes the parity
// `nested_only` and `nested_ignored_under_root_marker` fixtures
// behave identically on both sides — the inner package.json is
// nested on both sides, so both detectors ignore it the same
// way.
func seedBoth(t *testing.T, files []string) (fsys fs.FS, tarballPath string) {
	t.Helper()
	dir := t.TempDir()
	for _, f := range files {
		p := filepath.Join(dir, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	tarDir := t.TempDir()
	tarballPath = filepath.Join(tarDir, "src.tar.gz")
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range files {
		hdr := &tar.Header{
			Name:     filepath.FromSlash(name),
			Mode:     0o644,
			Size:     0,
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	if err := os.WriteFile(tarballPath, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write tarball: %v", err)
	}
	return os.DirFS(dir), tarballPath
}

// seedBothWithFiles is the content-aware variant used by
// TestVersionParity.
func seedBothWithFiles(t *testing.T, files map[string]string) (fsys fs.FS, tarballPath string) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	tarDir := t.TempDir()
	tarballPath = filepath.Join(tarDir, "src.tar.gz")
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		hdr := &tar.Header{
			Name:     filepath.FromSlash(name),
			Mode:     0o644,
			Size:     int64(len(body)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header %s: %v", name, err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("tar write %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	if err := os.WriteFile(tarballPath, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write tarball: %v", err)
	}
	return os.DirFS(dir), tarballPath
}

// TestDetectCLIParity is the load-bearing acceptance gate for
// PROV-2 / ADR-088. It runs the same fixture through both
// detection paths (CLI's fs.FS and server's tarball) and
// asserts that BOTH return the same framework and that the
// framework matches the expected one.
//
// A regression in the priority order (Docker dropped from the
// front of appMarkers, or Python reordered after Go) would
// flip one of these subtests. A regression in case-insensitive
// matching would flip the case_insensitive_* cases. A
// regression in nested-ignored would flip the nested_ignored
// case. A regression that re-introduces handler.* as a
// marker would flip handler_only.
func TestDetectCLIParity(t *testing.T) {
	cases := []struct {
		name  string
		files []string
		want  markers.Framework
	}{
		// Empty / non-marker — both sides return unknown.
		{"empty", nil, markers.FrameworkUnknown},
		{"readme_only", []string{"README.md", "notes.txt"}, markers.FrameworkUnknown},
		{"nested_only", []string{"apps/web/package.json"}, markers.FrameworkUnknown},
		{"handler_only", []string{"handler.js"}, markers.FrameworkUnknown},
		{"handler_plus_readme", []string{"handler.js", "README.md"}, markers.FrameworkUnknown},

		// Single-marker pins.
		{"dockerfile_alone", []string{"Dockerfile"}, markers.FrameworkDocker},
		{"node_only", []string{"package.json"}, markers.FrameworkNode},
		{"python_requirements", []string{"requirements.txt"}, markers.FrameworkPython},
		{"python_pyproject", []string{"pyproject.toml"}, markers.FrameworkPython},
		{"python_pipfile", []string{"Pipfile"}, markers.FrameworkPython},
		{"python_setup", []string{"setup.py"}, markers.FrameworkPython},
		{"go_only", []string{"go.mod"}, markers.FrameworkGo},

		// Case-insensitive pins (CI failures on macOS APFS —
		// see pack.go:200-213 prior regression).
		{"case_insensitive_dockerfile", []string{"dockerfile"}, markers.FrameworkDocker},
		{"case_insensitive_package", []string{"Package.JSON"}, markers.FrameworkNode},
		{"case_insensitive_go", []string{"GO.MOD"}, markers.FrameworkGo},

		// Priority order — Dockerfile first.
		{"dockerfile_beats_node", []string{"Dockerfile", "package.json"}, markers.FrameworkDocker},
		{"dockerfile_beats_go", []string{"Dockerfile", "go.mod"}, markers.FrameworkDocker},
		{"dockerfile_beats_python", []string{"Dockerfile", "requirements.txt"}, markers.FrameworkDocker},
		{"node_beats_python", []string{"package.json", "requirements.txt"}, markers.FrameworkNode},
		{"node_beats_go", []string{"package.json", "go.mod"}, markers.FrameworkNode},
		{"python_beats_go", []string{"go.mod", "requirements.txt"}, markers.FrameworkPython},

		// Nested-marker ignored — the inner package.json is not
		// a project marker.
		{"nested_ignored_under_root_marker", []string{
			"apps/web/package.json",
			"requirements.txt",
		}, markers.FrameworkPython},

		// Multi-Python-marker — first hit wins (priority order
		// among the four Python markers is pyproject.toml
		// first among those reaching the same framework,
		// because requirements.txt is listed first in
		// appMarkers).
		{"all_python_markers", []string{
			"requirements.txt",
			"pyproject.toml",
			"Pipfile",
			"setup.py",
		}, markers.FrameworkPython},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fsys, tarball := seedBoth(t, tc.files)
			cliFW, cliErr := markers.DetectFromFS(fsys)
			srvFW, srvErr := markers.DetectFromTarball(tarball)
			// Both sides must agree on the framework. This is
			// the parity contract.
			if cliFW != srvFW {
				t.Errorf("PARITY DRIFT: CLI=%q server=%q want=%q",
					cliFW, srvFW, tc.want)
			}
			// And both must match the expected.
			if cliFW != tc.want {
				t.Errorf("DetectFromFS = %q, want %q", cliFW, tc.want)
			}
			if srvFW != tc.want {
				t.Errorf("DetectFromTarball = %q, want %q", srvFW, tc.want)
			}
			// Both sides should treat unknown as a normal
			// return value (not an error). The CLI uses the
			// (unknown, nil) tuple to signal "no marker found"
			// — the caller decides whether that's an error.
			if tc.want == markers.FrameworkUnknown {
				if cliErr != nil {
					t.Errorf("DetectFromFS(unknown) error = %v, want nil", cliErr)
				}
				if srvErr != nil {
					t.Errorf("DetectFromTarball(unknown) error = %v, want nil", srvErr)
				}
			}
		})
	}
}

// TestVersionParity asserts that VersionFromFS(fsys) and
// VersionFromTarball(tarball) return the same version string
// for the same source. The 5 cases below cover every active
// framework (node, python, go) plus the empty-version case
// (docker, unknown).
func TestVersionParity(t *testing.T) {
	cases := []struct {
		name     string
		fw       markers.Framework
		files    map[string]string
		wantNotEmpty bool
	}{
		{"node_nvmrc", markers.FrameworkNode, map[string]string{
			".nvmrc":       "22.11.0",
			"package.json": "{}",
		}, true},
		{"node_engines_caret", markers.FrameworkNode, map[string]string{
			"package.json": `{"engines":{"node":"^22.11.0"}}`,
		}, true},
		{"python_version", markers.FrameworkPython, map[string]string{
			".python-version": "3.11.0",
		}, true},
		{"python_requires", markers.FrameworkPython, map[string]string{
			"pyproject.toml": `requires-python = ">=3.13"`,
		}, true},
		{"go_directive", markers.FrameworkGo, map[string]string{
			"go.mod": "module x\n\ngo 1.24\n",
		}, true},
		{"docker_no_version", markers.FrameworkDocker, map[string]string{
			"Dockerfile": "FROM alpine:3.20\n",
		}, false},
		{"unknown_no_version", markers.FrameworkUnknown, map[string]string{
			".nvmrc": "22.11.0",
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fsys, tarball := seedBothWithFiles(t, tc.files)
			fsVer := markers.VersionFromFS(fsys, tc.fw)
			tarVer := markers.VersionFromTarball(tarball, tc.fw)
			if fsVer != tarVer {
				t.Errorf("VERSION PARITY DRIFT: FS=%q Tarball=%q", fsVer, tarVer)
			}
			if tc.wantNotEmpty && fsVer == "" {
				t.Errorf("VersionFromFS = %q, want non-empty", fsVer)
			}
			if !tc.wantNotEmpty && fsVer != "" {
				t.Errorf("VersionFromFS = %q, want \"\"", fsVer)
			}
		})
	}
}

// TestMarkerForAndIsAppMarker pin the single-name lookup API
// (used by the CLI's detectNestedMarkerHint + detectShape-
// inline-switch replacement). The slice-order pin lives in
// TestMarkers_PriorityOrder in detect_test.go.
func TestMarkerForAndIsAppMarker(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantFW  markers.Framework
		wantIs  bool
	}{
		{"Dockerfile", "Dockerfile", markers.FrameworkDocker, true},
		{"dockerfile_lower", "dockerfile", markers.FrameworkDocker, true},
		{"DOCKERFILE_upper", "DOCKERFILE", markers.FrameworkDocker, true},
		{"package.json", "package.json", markers.FrameworkNode, true},
		{"PACKAGE.JSON", "PACKAGE.JSON", markers.FrameworkNode, true},
		{"requirements.txt", "requirements.txt", markers.FrameworkPython, true},
		{"pyproject.toml", "pyproject.toml", markers.FrameworkPython, true},
		{"Pipfile", "Pipfile", markers.FrameworkPython, true},
		{"setup.py", "setup.py", markers.FrameworkPython, true},
		{"go.mod", "go.mod", markers.FrameworkGo, true},
		{"handler.js_unknown", "handler.js", markers.FrameworkUnknown, false},
		{"README_unknown", "README.md", markers.FrameworkUnknown, false},
		{"index.js_unknown", "index.js", markers.FrameworkUnknown, false},
		{"empty_unknown", "", markers.FrameworkUnknown, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := markers.MarkerFor(tc.input); got != tc.wantFW {
				t.Errorf("MarkerFor(%q) = %q, want %q", tc.input, got, tc.wantFW)
			}
			if got := markers.IsAppMarker(tc.input); got != tc.wantIs {
				t.Errorf("IsAppMarker(%q) = %v, want %v", tc.input, got, tc.wantIs)
			}
		})
	}
}

// TestMarkers_PriorityOrderExternal pins the package-private
// priority order from the external test side. The slice-order
// test inside the package (markers_test.go::TestMarkers_
// PriorityOrder) is reluctantly useless against an external
// mutation; the order is exported as Markers() so any
// accidental re-shuffle is visible here.
func TestMarkers_PriorityOrderExternal(t *testing.T) {
	got := markers.Markers()
	want := []string{
		"Dockerfile",
		"package.json",
		"requirements.txt",
		"pyproject.toml",
		"Pipfile",
		"setup.py",
		"go.mod",
	}
	if len(got) != len(want) {
		t.Fatalf("len(markers) = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("markers[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
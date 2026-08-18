//go:build linux

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/onebox-faas/faas/pkg/api"
)

// TestDecideMode_BuildBeatsApp covers the precedence rule: a build manifest
// wins when both are present (defensive — base images normally carry at
// most one, but a misconfig shouldn't be a silent regression).
func TestDecideMode_BuildBeatsApp(t *testing.T) {
	fsys := fstest.MapFS{
		"etc/faas/build.json": &fstest.MapFile{Data: mustMarshal(t, api.BuildManifest{BuildID: "b", Framework: api.FrameworkRailpackNode, TimeoutSec: 60})},
		"etc/faas/app.json":   &fstest.MapFile{Data: []byte(`{"kind":"app"}`)},
	}
	mode, m, err := decideMode(fsys)
	if err != nil {
		t.Fatalf("decideMode: %v", err)
	}
	if mode != modeBuild {
		t.Fatalf("mode = %v, want modeBuild (build should beat app)", mode)
	}
	if m.BuildID != "b" || m.Framework != api.FrameworkRailpackNode || m.TimeoutSec != 60 {
		t.Errorf("manifest round-trip mismatch: %+v", m)
	}
}

func TestDecideMode_AppOnly(t *testing.T) {
	fsys := fstest.MapFS{
		"etc/faas/app.json": &fstest.MapFile{Data: []byte(`{}`)},
	}
	mode, _, err := decideMode(fsys)
	if err != nil {
		t.Fatalf("decideMode: %v", err)
	}
	if mode != modeApp {
		t.Errorf("mode = %v, want modeApp", mode)
	}
}

func TestDecideMode_BuildOnly(t *testing.T) {
	fsys := fstest.MapFS{
		"etc/faas/build.json": &fstest.MapFile{Data: mustMarshal(t, api.BuildManifest{BuildID: "b"})},
	}
	mode, m, err := decideMode(fsys)
	if err != nil {
		t.Fatalf("decideMode: %v", err)
	}
	if mode != modeBuild {
		t.Errorf("mode = %v, want modeBuild", mode)
	}
	if m.BuildID != "b" {
		t.Errorf("BuildID = %q, want b", m.BuildID)
	}
}

func TestDecideMode_BadJSONFallsBackToApp(t *testing.T) {
	fsys := fstest.MapFS{
		"etc/faas/build.json": &fstest.MapFile{Data: []byte(`{not json`)},
	}
	mode, _, _ := decideMode(fsys)
	if mode != modeApp {
		t.Errorf("mode = %v, want modeApp (garbage build.json must not panic)", mode)
	}
}

func TestEnsureResolverFile_PreservesUsableConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resolv.conf")
	want := "nameserver 10.0.0.1\n"
	if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureResolverFile(path); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("resolver config changed: %q", got)
	}
}

func TestEnsureResolverFileCreatesFallbackAndReplacesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	path := filepath.Join(dir, "resolv.conf")
	if err := os.WriteFile(target, []byte("target"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := ensureResolverFile(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("resolver path remained a symlink")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !hasNameserver(got) {
		t.Fatalf("fallback has no nameserver: %q", got)
	}
	targetGot, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(targetGot) != "target" {
		t.Fatalf("symlink target was modified: %q", targetGot)
	}
}

// TestClassifyExitCodes is the canonical exit-code → FailureClass mapping.
// builderd's ProcessOne consumes these strings.
func TestClassifyExitCodes(t *testing.T) {
	cases := []struct {
		code int
		want string
	}{
		{0, ""},
		{1, "FailureUserError"},
		{124, "FailureTimeout"},
		{137, "FailureOOM"},
		{-1, "FailureUserError"},
		{42, "FailureUserError"},
	}
	for _, c := range cases {
		if got := classify(c.code); got != c.want {
			t.Errorf("classify(%d) = %q, want %q", c.code, got, c.want)
		}
	}
}

// TestTailOf covers the LogTailBytes clamp.
func TestTailOf(t *testing.T) {
	if got := tailOf([]byte("hello"), 100); got != "hello" {
		t.Errorf("tailOf short = %q", got)
	}
	if got := tailOf([]byte("0123456789abcdef"), 4); got != "cdef" {
		t.Errorf("tailOf(long, 4) = %q, want cdef", got)
	}
	if got := tailOf([]byte("hello"), 0); got != "hello" {
		t.Errorf("tailOf(long, 0) = %q, want full", got)
	}
}

// TestBuildDoneShape round-trips a representative BuildDone payload through
// JSON to verify the field names match what builderd consumes. The actual
// writeAndPoweroff path is covered by the metal-loop integration test
// (`make metal-lima`); here we just lock the wire shape.
func TestBuildDoneShape(t *testing.T) {
	done := api.BuildDone{
		SchemaVersion: 1,
		BuildID:       "b-shape",
		ExitCode:      137,
		OCIImagePath:  "/build/out/image.tar",
		LogTail:       "step 1: ..., step 2: ...",
		FailureClass:  "FailureOOM",
	}
	data, err := json.Marshal(done)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got api.BuildDone
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.BuildID != "b-shape" || got.ExitCode != 137 || got.FailureClass != "FailureOOM" || got.OCIImagePath != "/build/out/image.tar" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestFlattenSingleSourceDir(t *testing.T) {
	workdir := t.TempDir()
	root := filepath.Join(workdir, "hello-node")
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"name":"hello"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "index.js"), []byte("console.log('ok')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := flattenSingleSourceDir(workdir); err != nil {
		t.Fatalf("flattenSingleSourceDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workdir, "package.json")); err != nil {
		t.Fatalf("package.json not promoted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workdir, "src", "index.js")); err != nil {
		t.Fatalf("nested source not promoted: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("archive root still exists: err=%v", err)
	}
}

func TestStageExecutable_CopiesAndMarksExecutable(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	target := filepath.Join(dir, "nested", "mise")
	want := []byte("mise-musl")
	if err := os.WriteFile(source, want, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := stageExecutable(source, target); err != nil {
		t.Fatalf("stageExecutable: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("target = %q, want %q", got, want)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("target mode = %o, want 755", info.Mode().Perm())
	}
}

func TestFlattenSingleSourceDirLeavesRootFiles(t *testing.T) {
	workdir := t.TempDir()
	if err := os.Mkdir(filepath.Join(workdir, "hello-node"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "package.json"), []byte(`{"name":"hello"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := flattenSingleSourceDir(workdir); err != nil {
		t.Fatalf("flattenSingleSourceDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workdir, "hello-node")); err != nil {
		t.Fatalf("root directory unexpectedly changed: %v", err)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestBuildArgv pins the in-VM build-engine argv shape. builderd relies
// on the (framework → argv) mapping to render BuildDone.FailureClass
// correctly (the LogTail carries railpack / buildctl output verbatim,
// so the binary name is the operator's grep target). A regression that
// adds a new BuildFramework without extending this table would silently
// land in the `default` (auto) branch and produce a non-Railpack-aware
// log tail that the customer can't act on.
func TestBuildArgv(t *testing.T) {
	workdir := "/build/src"
	outdir := "/build/out"
	cases := []struct {
		name string
		fw   api.BuildFramework
		want []string
	}{
		{
			name: "dockerfile → buildctl",
			fw:   api.FrameworkDockerfile,
			want: []string{
				"/usr/local/bin/buildctl", "build",
				"--frontend", "dockerfile",
				"--local", "context=" + workdir,
				"--local", "dockerfile=" + workdir,
				"--output", "type=oci,dest=" + outdir + "/image.tar",
			},
		},
		{
			name: "node → railpack",
			fw:   api.FrameworkRailpackNode,
			want: []string{"/bin/sh", "-c", "/usr/local/bin/railpack prepare '/build/src' --plan-out '/build/railpack-plan.json' --info-out '/build/railpack-info.json' --hide-pretty-plan && exec /usr/local/bin/buildctl build --frontend gateway.v0 --opt source=ghcr.io/railwayapp/railpack-frontend:latest --opt filename=railpack-plan.json --local context='/build/src' --local dockerfile='/build' --output type=oci,dest='/build/out/image.tar' --progress plain"},
		},
		{
			name: "python → railpack",
			fw:   api.FrameworkRailpackPython,
			want: []string{"/bin/sh", "-c", "/usr/local/bin/railpack prepare '/build/src' --plan-out '/build/railpack-plan.json' --info-out '/build/railpack-info.json' --hide-pretty-plan && exec /usr/local/bin/buildctl build --frontend gateway.v0 --opt source=ghcr.io/railwayapp/railpack-frontend:latest --opt filename=railpack-plan.json --local context='/build/src' --local dockerfile='/build' --output type=oci,dest='/build/out/image.tar' --progress plain"},
		},
		{
			name: "go → railpack",
			fw:   api.FrameworkRailpackGo,
			want: []string{"/bin/sh", "-c", "/usr/local/bin/railpack prepare '/build/src' --plan-out '/build/railpack-plan.json' --info-out '/build/railpack-info.json' --hide-pretty-plan && exec /usr/local/bin/buildctl build --frontend gateway.v0 --opt source=ghcr.io/railwayapp/railpack-frontend:latest --opt filename=railpack-plan.json --local context='/build/src' --local dockerfile='/build' --output type=oci,dest='/build/out/image.tar' --progress plain"},
		},
		{
			name: "auto → railpack (default branch)",
			fw:   api.FrameworkAuto,
			want: []string{"/bin/sh", "-c", "/usr/local/bin/railpack prepare '/build/src' --plan-out '/build/railpack-plan.json' --info-out '/build/railpack-info.json' --hide-pretty-plan && exec /usr/local/bin/buildctl build --frontend gateway.v0 --opt source=ghcr.io/railwayapp/railpack-frontend:latest --opt filename=railpack-plan.json --local context='/build/src' --local dockerfile='/build' --output type=oci,dest='/build/out/image.tar' --progress plain"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildArgv(api.BuildManifest{
				Framework: tc.fw,
				Workdir:   workdir,
				OutDir:    outdir,
			})
			if !equalSlice(got, tc.want) {
				t.Errorf("buildArgv(%q) = %v, want %v", tc.fw, got, tc.want)
			}
		})
	}
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

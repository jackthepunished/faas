package main

// Test file for issue #737 / ADR-083 — the customer-visible
// "Detected: …" CLI print line is the load-bearing acceptance gate
// for the zero-config function vs app deploy auto-detection. The
// test drives resolveDeployShape directly (no apid, no auth) and
// captures osStdout via the same swap used in commands_metrics_test.go
// / commands_usage_summary_test.go, then asserts the print line.
//
// The test is whitebox (package main) on purpose: the print +
// detect + infer triad lives in pack.go next to detectShape /
// inferFunctionRuntime, so the unit-test seam is local. We don't
// shell out to `go run ./cmd/gregale` because that would require an
// authenticated apid at the other end — out of scope for the print
// line, which is the only new customer-visible behaviour.

import (
	"bytes"
	"strings"
	"testing"
)

// TestResolveDeployShape_Function is the headline case: a cwd
// containing only handler.js must produce the
// "Detected: function, runtime=node22, handler=handler.handler"
// line on stdout. A regression that drops the print line, picks
// the wrong shape, picks the wrong runtime, or moves the print
// line after the multipart POST fails this gate.
func TestResolveDeployShape_Function(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "handler.js", "exports.handler = async () => ({statusCode:200, body:'ok'});")

	var buf bytes.Buffer
	oldOut := osStdout
	osStdout = &buf
	defer func() { osStdout = oldOut }()

	sh, rt, hnd, err := resolveDeployShape(dir, false, false)
	if err != nil {
		t.Fatalf("resolveDeployShape: %v", err)
	}
	if sh != shapeFunction {
		t.Errorf("shape = %d, want %d (shapeFunction)", sh, shapeFunction)
	}
	if rt != "node22" {
		t.Errorf("runtime = %q, want %q", rt, "node22")
	}
	if hnd != "handler.handler" {
		t.Errorf("handler = %q, want %q", hnd, "handler.handler")
	}
	got := buf.String()
	wantLine := "Detected: function, runtime=node22, handler=handler.handler"
	if !strings.Contains(got, wantLine) {
		t.Errorf("stdout missing %q; got %q", wantLine, got)
	}
}

// TestResolveDeployShape_App pins the app-shape print line: a cwd
// containing package.json must produce the
// "Detected: app, framework=node" line. The framework name comes
// from detectFramework, kept here only to pin the format string
// shape.
func TestResolveDeployShape_App(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "package.json", "{}")

	var buf bytes.Buffer
	oldOut := osStdout
	osStdout = &buf
	defer func() { osStdout = oldOut }()

	sh, _, _, err := resolveDeployShape(dir, false, false)
	if err != nil {
		t.Fatalf("resolveDeployShape: %v", err)
	}
	if sh != shapeApp {
		t.Errorf("shape = %d, want %d (shapeApp)", sh, shapeApp)
	}
	got := buf.String()
	wantLine := "Detected: app, framework=node"
	if !strings.Contains(got, wantLine) {
		t.Errorf("stdout missing %q; got %q", wantLine, got)
	}
}

// TestResolveDeployShape_Unknown pins the no-source error path: an
// empty / README-only cwd must surface an actionable error that
// mentions BOTH app and function paths, NOT silently pick app.
func TestResolveDeployShape_Unknown(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "README.md", "hi")

	var buf bytes.Buffer
	oldOut := osStdout
	osStdout = &buf
	defer func() { osStdout = oldOut }()

	sh, _, _, err := resolveDeployShape(dir, false, false)
	if err == nil {
		t.Fatalf("resolveDeployShape on empty dir should error; got shape=%d", sh)
	}
	if sh != shapeUnknown {
		t.Errorf("shape = %d, want %d (shapeUnknown)", sh, shapeUnknown)
	}
	if !strings.Contains(err.Error(), "package.json") {
		t.Errorf("error should mention the app-marker path; got %v", err)
	}
	if !strings.Contains(err.Error(), "handler.{js,ts,py,go}") {
		t.Errorf("error should mention the function path; got %v", err)
	}
	// The unknown path emits NO "Detected:" line — the print is
	// only on a successful shape resolve. Pins that the gate is
	// well-defined.
	if strings.Contains(buf.String(), "Detected:") {
		t.Errorf("stdout should not contain Detected line on shapeUnknown; got %q", buf.String())
	}
}

// TestResolveDeployShape_ExplicitFunctionForcesFunction pins the
// --function short-circuit: even a cwd that has package.json (an
// app marker) must resolve to shapeFunction when explicitFunction
// is true. The customer paid for the explicit flag — auto-detection
// must NOT overrule them.
func TestResolveDeployShape_ExplicitFunctionForcesFunction(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "package.json", "{}")
	writeFile(t, dir, "handler.js", "")

	var buf bytes.Buffer
	oldOut := osStdout
	osStdout = &buf
	defer func() { osStdout = oldOut }()

	sh, rt, hnd, err := resolveDeployShape(dir, true, false)
	if err != nil {
		t.Fatalf("resolveDeployShape(--function): %v", err)
	}
	if sh != shapeFunction {
		t.Errorf("shape = %d, want %d (shapeFunction via --function)", sh, shapeFunction)
	}
	if rt != "node22" {
		t.Errorf("runtime = %q, want %q", rt, "node22")
	}
	if hnd != "handler.handler" {
		t.Errorf("handler = %q, want %q", hnd, "handler.handler")
	}
	if !strings.Contains(buf.String(), "Detected: function, runtime=node22, handler=handler.handler") {
		t.Errorf("stdout missing function print line; got %q", buf.String())
	}
}

// TestResolveDeployShape_ExplicitAppForcesApp pins the --app
// short-circuit: even a cwd that has only handler.js (no app
// markers) must resolve to shapeApp when explicitApp is true.
// Symmetric to the function test above.
func TestResolveDeployShape_ExplicitAppForcesApp(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "handler.js", "")

	var buf bytes.Buffer
	oldOut := osStdout
	osStdout = &buf
	defer func() { osStdout = oldOut }()

	sh, _, _, err := resolveDeployShape(dir, false, true)
	if err != nil {
		t.Fatalf("resolveDeployShape(--app): %v", err)
	}
	if sh != shapeApp {
		t.Errorf("shape = %d, want %d (shapeApp via --app)", sh, shapeApp)
	}
	if !strings.Contains(buf.String(), "Detected: app") {
		t.Errorf("stdout missing app print line; got %q", buf.String())
	}
}

// TestResolveDeployShape_FunctionRuntimes pins each extension in
// the runtime map (.js / .ts → node22, .py → python312,
// .go → go124). The handler wire value stays the literal
// "handler.handler" across all four — matching the function-*
// template convention (cmd/gregale/templates/function-node/handler.js).
func TestResolveDeployShape_FunctionRuntimes(t *testing.T) {
	cases := []struct {
		name    string
		handler string
		wantRt  string
	}{
		{"node_js", "handler.js", "node22"},
		{"node_ts", "handler.ts", "node22"},
		{"python", "handler.py", "python312"},
		{"go", "handler.go", "go124"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, tc.handler, "")

			var buf bytes.Buffer
			oldOut := osStdout
			osStdout = &buf
			defer func() { osStdout = oldOut }()

			sh, rt, hnd, err := resolveDeployShape(dir, false, false)
			if err != nil {
				t.Fatalf("resolveDeployShape: %v", err)
			}
			if sh != shapeFunction {
				t.Errorf("shape = %d, want %d (shapeFunction)", sh, shapeFunction)
			}
			if rt != tc.wantRt {
				t.Errorf("runtime = %q, want %q", rt, tc.wantRt)
			}
			if hnd != "handler.handler" {
				t.Errorf("handler = %q, want %q", hnd, "handler.handler")
			}
			wantSub := "Detected: function, runtime=" + tc.wantRt + ", handler=handler.handler"
			if !strings.Contains(buf.String(), wantSub) {
				t.Errorf("stdout missing %q; got %q", wantSub, buf.String())
			}
		})
	}
}

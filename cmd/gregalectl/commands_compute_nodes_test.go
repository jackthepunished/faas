// commands_compute_nodes_test.go — whitebox tests for the
// operator-side `gregalectl compute-nodes add` subcommand
// (Tier 1 multi-host scale-out gap #1; PR-A).
//
// The four sibling subcommands (drain / drain-status / activate /
// force-drain) are exercised by cmd/deployctl/upgrade_test.go and
// the cmd/e2e suite; add is new and lives here.
//
// Test conventions:
//   - The state seam (computeNodesStoreOpener) hands back a single
//     MemStore + a no-op close for the lifetime of each test. The
//     seam returns the SAME store across calls so the second
//     upsert in TestCmdComputeNodesAdd_Idempotent exercises the
//     real ON CONFLICT path.
//   - stdout/stderr capture: we redirect with os.Pipe(); the
//     helper restore() runs the io.Copy to drain the buffer. Tests
//     call restore() before they read the buffer (else writes
//     remain in the pipe).

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/state"
)

// TestMain wires the MemStore seam before any test runs. Each
// test re-wires its OWN seam with a fresh MemStore (see
// resetMemStore) — TestMain only installs a default so a stray
// test that forgets to reset still produces a deterministic
// failure instead of a nil-pointer panic on the typed-nil pool.
func TestMain(m *testing.M) {
	setComputeNodesStoreOpener(func() (state.Store, func(), error) {
		return state.NewMemStore(), func() {}, nil
	})
	os.Exit(m.Run())
}

// resetMemStore re-wires the seam so successive calls inside the
// same test return the SAME MemStore. Each call must hit the
// shared store, not a new one, so the second upsert is an UPDATE
// not an INSERT.
func resetMemStore(t *testing.T) {
	t.Helper()
	st := state.NewMemStore()
	setComputeNodesStoreOpener(func() (state.Store, func(), error) {
		return st, func() {}, nil
	})
}

// captureStderrComputeNodes runs fn with os.Stderr redirected to
// a buffer, returning whatever was written. Renamed to avoid
// collision with the package-wide captureStderr at
// commands_release_sbom_gate_test.go:107.
func captureStderrComputeNodes(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()
	fn()
	_ = w.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("read: %v", err)
	}
	return buf.String()
}

// captureStdoutComputeNodes runs fn with os.Stdout redirected; the
// returned restore() drains the pipe into buf BEFORE returning.
// Tests must invoke restore() and THEN read buf — otherwise the
// pipe buffer is still holding the bytes.
//
// Pattern:
//
//	buf, restore := captureStdoutComputeNodes(t)
//	restore()
//	...asserts on buf.String()...
func captureStdoutComputeNodes(t *testing.T, fn func()) (*bytes.Buffer, func()) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	fn()
	os.Stdout = orig
	_ = w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	_ = r.Close()
	return &buf, func() {}
}

// TestCmdComputeNodesAdd_MissingFlags asserts the usage path for
// every required flag. Mirrors the apid handler's 400 surface.
func TestCmdComputeNodesAdd_MissingFlags(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "missing all flags",
			args:    []string{},
			wantErr: "--name required",
		},
		{
			name:    "missing target-url",
			args:    []string{"--name=fsn-3", "--vpcpus=160", "--mem-mb=56000", "--max-concurrency=200", "--admission-ceiling-mb=47600"},
			wantErr: "--target-url required",
		},
		{
			name:    "missing capacity",
			args:    []string{"--name=fsn-3", "--target-url=tcp://vmmd-3.faas:50051"},
			wantErr: "must all be > 0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetMemStore(t)
			stderr := captureStderrComputeNodes(t, func() {
				if code := cmdComputeNodesAdd(tc.args); code != 2 {
					t.Errorf("cmdComputeNodesAdd(%v) = %d, want 2 (usage)", tc.args, code)
				}
			})
			if !strings.Contains(stderr, tc.wantErr) {
				t.Errorf("stderr missing %q: got %q", tc.wantErr, stderr)
			}
		})
	}
}

// TestCmdComputeNodesAdd_BadTargetURL covers the target_url
// validation rules.
func TestCmdComputeNodesAdd_BadTargetURL(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantErr string
	}{
		{"loopback-ipv4", "tcp://127.0.0.1:50051", "loopback"},
		{"any-address", "tcp://0.0.0.0:50051", "non-routable"},
		{"missing-port", "tcp://vmmd-3.faas", "missing port"},
		{"missing-scheme", "vmmd-3.faas:50051", "scheme must be"},
		{"unix-empty", "unix://", "with empty path"},
		{"dns-empty", "dns://", "with empty hostname"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetMemStore(t)
			stderr := captureStderrComputeNodes(t, func() {
				if code := cmdComputeNodesAdd([]string{
					"--name=fsn-3",
					"--target-url=" + tc.url,
					"--vpcpus=160", "--mem-mb=56000",
					"--max-concurrency=200", "--admission-ceiling-mb=47600",
				}); code != 2 {
					t.Errorf("cmdComputeNodesAdd(%s) = %d, want 2", tc.url, code)
				}
			})
			if !strings.Contains(stderr, tc.wantErr) {
				t.Errorf("target_url %q: stderr missing %q, got %q", tc.url, tc.wantErr, stderr)
			}
		})
	}
}

// TestCmdComputeNodesAdd_BadName covers the name validator.
func TestCmdComputeNodesAdd_BadName(t *testing.T) {
	cases := []string{
		"-leading-dash",
		"trailing-dash-",
		"UPPERCASE",
		"has spaces",
		"has_underscore",
	}
	for _, name := range cases {
		t.Run("name="+name, func(t *testing.T) {
			resetMemStore(t)
			stderr := captureStderrComputeNodes(t, func() {
				if code := cmdComputeNodesAdd([]string{
					"--name=" + name,
					"--target-url=tcp://vmmd-3.faas:50051",
					"--vpcpus=160", "--mem-mb=56000",
					"--max-concurrency=200", "--admission-ceiling-mb=47600",
				}); code != 2 {
					t.Errorf("cmdComputeNodesAdd(name=%q) = %d, want 2", name, code)
				}
			})
			if !strings.Contains(stderr, "valid fqdn") {
				t.Errorf("name %q: stderr missing validator output, got %q", name, stderr)
			}
		})
	}
}

// TestCmdComputeNodesAdd_HappyPath asserts the canonical flow:
// all flags pass validation, the row lands in compute_nodes, the
// OK line lands on stdout, the pg_notify hint lands on stderr.
func TestCmdComputeNodesAdd_HappyPath(t *testing.T) {
	resetMemStore(t)
	var stdoutBuf *bytes.Buffer
	stderr := captureStderrComputeNodes(t, func() {
		stdoutBuf, _ = captureStdoutComputeNodes(t, func() {
			if code := cmdComputeNodesAdd([]string{
				"--name=fsn-3",
				"--target-url=tcp://vmmd-3.faas:50051",
				"--vpcpus=160", "--mem-mb=56000",
				"--max-concurrency=200", "--admission-ceiling-mb=47600",
			}); code != 0 {
				t.Errorf("cmdComputeNodesAdd(happy) = %d, want 0", code)
			}
		})
	})

	if !strings.Contains(stdoutBuf.String(), "OK name=fsn-3") {
		t.Errorf("stdout missing OK line: %q", stdoutBuf.String())
	}
	if !strings.Contains(stdoutBuf.String(), "target_url=tcp://vmmd-3.faas:50051") {
		t.Errorf("stdout missing target_url: %q", stdoutBuf.String())
	}
	if !strings.Contains(stdoutBuf.String(), "admission_ceiling_mb=47600") {
		t.Errorf("stdout missing admission ceiling: %q", stdoutBuf.String())
	}
	if !strings.Contains(stderr, "compute_node_changed pg_notify fired") {
		t.Errorf("stderr missing pg_notify hint: %q", stderr)
	}
}

// TestCmdComputeNodesAdd_HappyPath_JSON asserts the --json output
// shape is what the PR-B deploy add-node will consume.
func TestCmdComputeNodesAdd_HappyPath_JSON(t *testing.T) {
	resetMemStore(t)
	stdoutBuf, _ := captureStdoutComputeNodes(t, func() {
		captureStderrComputeNodes(t, func() {
			if code := cmdComputeNodesAdd([]string{
				"--name=fsn-3",
				"--target-url=tcp://vmmd-3.faas:50051",
				"--vpcpus=160", "--mem-mb=56000",
				"--max-concurrency=200", "--admission-ceiling-mb=47600",
				"--json",
			}); code != 0 {
				t.Errorf("cmdComputeNodesAdd(--json) = %d, want 0", code)
			}
		})
	})

	var got map[string]interface{}
	if err := json.Unmarshal(bytes.TrimSpace(stdoutBuf.Bytes()), &got); err != nil {
		t.Fatalf("json parse: %v\nbody: %q", err, stdoutBuf.String())
	}
	if got["name"] != "fsn-3" {
		t.Errorf("name = %v, want fsn-3", got["name"])
	}
	if got["target_url"] != "tcp://vmmd-3.faas:50051" {
		t.Errorf("target_url = %v", got["target_url"])
	}
	if got["active"] != true {
		t.Errorf("active = %v, want true", got["active"])
	}
}

// TestCmdComputeNodesAdd_FromFile asserts the PR-B bridge: a JSON
// payload file drives the same upsert without per-field flags.
func TestCmdComputeNodesAdd_FromFile(t *testing.T) {
	resetMemStore(t)
	dir := t.TempDir()
	payload := `{
	  "name": "fsn-3",
	  "target_url": "tcp://vmmd-3.faas:50051",
	  "vpcpus": 160,
	  "mem_mb": 56000,
	  "max_concurrency": 200,
	  "admission_ceiling_mb": 47600
	}`
	payloadPath := filepath.Join(dir, "payload.json")
	if err := os.WriteFile(payloadPath, []byte(payload), 0o644); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	stdoutBuf, _ := captureStdoutComputeNodes(t, func() {
		captureStderrComputeNodes(t, func() {
			if code := cmdComputeNodesAdd([]string{"--from-file=" + payloadPath}); code != 0 {
				t.Errorf("cmdComputeNodesAdd(--from-file) = %d, want 0", code)
			}
		})
	})

	if !strings.Contains(stdoutBuf.String(), "OK name=fsn-3") {
		t.Errorf("stdout missing OK line: %q", stdoutBuf.String())
	}
}

// TestCmdComputeNodesAdd_FromFile_Missing asserts the missing-file
// path is exit 1 (platform error), distinct from exit 2 (usage).
func TestCmdComputeNodesAdd_FromFile_Missing(t *testing.T) {
	resetMemStore(t)
	stderr := captureStderrComputeNodes(t, func() {
		if code := cmdComputeNodesAdd([]string{"--from-file=/nonexistent/payload.json"}); code != 1 {
			t.Errorf("cmdComputeNodesAdd(missing file) = %d, want 1", code)
		}
	})
	if !strings.Contains(stderr, "read --from-file") {
		t.Errorf("stderr missing read error: %q", stderr)
	}
}

// TestCmdComputeNodesAdd_FromFile_MalformedJSON asserts the bad-JSON
// path is exit 3 (platform error), matching the release kgv exit
// codes for parse errors.
func TestCmdComputeNodesAdd_FromFile_MalformedJSON(t *testing.T) {
	resetMemStore(t)
	dir := t.TempDir()
	badPath := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(badPath, []byte("not valid json"), 0o644); err != nil {
		t.Fatalf("write bad: %v", err)
	}
	stderr := captureStderrComputeNodes(t, func() {
		if code := cmdComputeNodesAdd([]string{"--from-file=" + badPath}); code != 3 {
			t.Errorf("cmdComputeNodesAdd(bad json) = %d, want 3", code)
		}
	})
	if !strings.Contains(stderr, "parse --from-file") {
		t.Errorf("stderr missing parse error: %q", stderr)
	}
}

// TestCmdComputeNodesAdd_Idempotent asserts a re-POST with the same
// name UPSERTs (preserves ID, refreshes capacity).
func TestCmdComputeNodesAdd_Idempotent(t *testing.T) {
	resetMemStore(t)
	commonArgs := []string{
		"--name=fsn-3",
		"--target-url=tcp://vmmd-3.faas:50051",
		"--vpcpus=160", "--mem-mb=56000",
		"--max-concurrency=200", "--admission-ceiling-mb=47600",
	}
	buf1, _ := captureStdoutComputeNodes(t, func() {
		captureStderrComputeNodes(t, func() {
			if code := cmdComputeNodesAdd(commonArgs); code != 0 {
				t.Fatalf("first add: %d", code)
			}
		})
	})
	firstLine := buf1.String()

	buf2, _ := captureStdoutComputeNodes(t, func() {
		captureStderrComputeNodes(t, func() {
			if code := cmdComputeNodesAdd(commonArgs); code != 0 {
				t.Fatalf("second add: %d", code)
			}
		})
	})
	secondLine := buf2.String()

	firstID := extractIDFromOK(firstLine)
	secondID := extractIDFromOK(secondLine)
	if firstID == "" {
		t.Fatalf("could not extract id from first OK line: %q", firstLine)
	}
	if secondID == "" {
		t.Fatalf("could not extract id from second OK line: %q", secondLine)
	}
	if firstID != secondID {
		t.Errorf("id changed across re-POST: first=%s second=%s", firstID, secondID)
	}
}

// extractIDFromOK pulls the id=... token from an OK line emitted by
// cmdComputeNodesAdd. Format: "OK name=X id=Y ...".
func extractIDFromOK(line string) string {
	const tag = " id="
	i := strings.Index(line, tag)
	if i < 0 {
		return ""
	}
	rest := line[i+len(tag):]
	end := strings.IndexAny(rest, " \n")
	if end < 0 {
		return rest
	}
	return rest[:end]
}

// _ pins the context import; harmless if unused.
var _ = context.Background

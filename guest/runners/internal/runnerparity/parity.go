// Package runnerparity owns the §4.9 envelope fixtures shared across the
// guest/runners/{node22,python312,go124} test suites. It is the first
// `internal/` package in the repo — Go's internal/ rule restricts import
// to packages rooted at guest/runners/, so the helper is reachable from
// each runner's main_test.go without changing production visibility.
//
// Production code MUST NOT import this package. The three runner main.go
// files stay in `package main` and are deliberately not extracted into a
// shared runtime package (see guest/runners/go124/main.go:9-14 — splitting
// keeps each runner buildable + lintable on its own). The parity helper
// is test-only: it owns the fake-script constants, the httptest server
// wiring, and the four post-handle assertions every runner's
// TestHandle_RoundTrip duplicates today.
package runnerparity

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// FakeHandler is a self-contained executable that reads the §4.9 envelope
// from stdin and writes back a response envelope (status=200,
// X-Echo-Method, X-Echo-Path, body_b64="echo:<path>"). The Interpreter
// slice drives RunRoundTrip's Content-Type policy and PATH skip guard.
type FakeHandler struct {
	// Script is the file contents to write to disk.
	Script string
	// Filename is the file name on disk (e.g. "handler.js", "handler.py",
	// "handler"). The runner's spawn argv uses this path verbatim.
	Filename string
	// Interpreter drives the spawn argv:
	//   - ["node"]     → `node <Filename>` (node22)
	//   - ["python3"]  → `python3 <Filename>` (python312)
	//   - nil          → exec the file directly (go124; POSIX shebang)
	// RunRoundTrip skips the test if any non-nil interpreter binary is
	// not on PATH — mirroring the per-runtime guards the three runners
	// already have today.
	Interpreter []string
}

// FakeNodeScript returns a FakeHandler that runs under `node`. The script
// echoes method + path back via the §4.9 response envelope. Skipped at
// test time if `node` is not on PATH.
func FakeNodeScript() FakeHandler {
	return FakeHandler{
		Script: `#!/usr/bin/env node
let buf = '';
process.stdin.on('data', (c) => { buf += c; });
process.stdin.on('end', () => {
  const env = JSON.parse(buf);
  const out = {
    status: 200,
    headers: { "X-Echo-Method": env.method, "X-Echo-Path": env.path },
    body_b64: Buffer.from("echo:" + env.path).toString("base64")
  };
  process.stdout.write(JSON.stringify(out));
});`,
		Filename:    "handler.js",
		Interpreter: []string{"node"},
	}
}

// FakePyScript returns a FakeHandler that runs under `python3`. The
// script echoes method + path back via the §4.9 response envelope.
// Skipped at test time if `python3` is not on PATH.
func FakePyScript() FakeHandler {
	return FakeHandler{
		Script: `#!/usr/bin/env python3
import sys, json, base64
env = json.loads(sys.stdin.read())
out = {
    "status": 200,
    "headers": {"X-Echo-Method": env["method"], "X-Echo-Path": env["path"]},
    "body_b64": base64.b64encode(("echo:" + env["path"]).encode()).decode(),
}
sys.stdout.write(json.dumps(out))
`,
		Filename:    "handler.py",
		Interpreter: []string{"python3"},
	}
}

// FakeGoScript returns a FakeHandler that is a POSIX-sh script exec'd
// directly — matches how go124 invokes its handler
// (guest/runners/go124/main.go:117). No interpreter required; POSIX-sh
// is on every Linux/macOS.
func FakeGoScript() FakeHandler {
	return FakeHandler{
		Script: `#!/bin/sh
read -r env
method=$(printf '%s' "$env" | sed -n 's/.*"method":"\([^"]*\)".*/\1/p')
path=$(printf '%s' "$env" | sed -n 's/.*"path":"\([^"]*\)".*/\1/p')
printf '{"status":200,"headers":{"X-Echo-Method":"%s","X-Echo-Path":"%s"},"body_b64":"%s"}' \
  "$method" "$path" "$(printf '%s' "echo:$path" | base64)"
`,
		Filename:    "handler",
		Interpreter: nil,
	}
}

// WriteMaterialize writes the FakeHandler's Script to a fresh temp file
// and returns the absolute path. The caller passes the returned path to
// its local `handle` function as the handler path. Helper-exposed so
// the per-runtime TestHandle_RoundTrip can keep the production
// visibility of `handle` — the runner's main_test.go wraps `handle` in
// a closure that captures this path.
func (f FakeHandler) WriteMaterialize(t *testing.T) string {
	t.Helper()
	if len(f.Interpreter) > 0 {
		if _, err := exec.LookPath(f.Interpreter[0]); err != nil {
			t.Skipf("%s not on PATH; skipping runtime round-trip", f.Interpreter[0])
		}
	}
	dir := t.TempDir()
	script := dir + "/" + f.Filename
	if err := os.WriteFile(script, []byte(f.Script), 0o755); err != nil {
		t.Fatalf("write handler: %v", err)
	}
	return script
}

// RunRoundTrip wires up an httptest.Server that delegates / to handler,
// fires GET /hello?x=1, and asserts: status=200, X-Echo-Method=="GET",
// X-Echo-Path=="/hello", body contains "echo:/hello". When
// fake.Interpreter == []string{"node"}, the helper also asserts
// Content-Type == "application/octet-stream" — node22's runtime
// override (guest/runners/node22/main.go:102). All other runtimes
// skip the Content-Type check; the python312/go124 runners pass through
// the handler's Content-Type verbatim.
//
// The handler closure is invoked with the materialized handler script
// path so the runner's per-package `handle` signature
// (func(w http.ResponseWriter, r *http.Request, handlerPath string))
// stays unchanged — production visibility of `handle` is preserved.
func RunRoundTrip(t *testing.T, fake FakeHandler, handler func(http.ResponseWriter, *http.Request, string)) {
	t.Helper()
	script := fake.WriteMaterialize(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		handler(w, r, script)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/hello?x=1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Echo-Method"); got != "GET" {
		t.Errorf("X-Echo-Method = %q, want GET", got)
	}
	if got := resp.Header.Get("X-Echo-Path"); got != "/hello" {
		t.Errorf("X-Echo-Path = %q, want /hello", got)
	}
	// node22's `handle` sets `Content-Type: application/octet-stream`
	// AFTER copying the handler's headers (guest/runners/node22/main.go:102).
	// python312 and go124 pass headers verbatim — skip the check.
	if len(fake.Interpreter) > 0 && fake.Interpreter[0] == "node" {
		if got := resp.Header.Get("Content-Type"); got != "application/octet-stream" {
			t.Errorf("Content-Type = %q, want application/octet-stream (node22 override)", got)
		}
	}
	body := new(bytes.Buffer)
	if _, err := io.Copy(body, resp.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(body.String(), "echo:/hello") {
		t.Errorf("body = %q, want contains echo:/hello", body.String())
	}
}

// AssertEnvelopeJSONTags marshals an envelope-shaped value and asserts
// the §4.9 JSON tags spell out method/path/headers/query/body_b64, and
// that the encoded body_b64 decodes back to wantBody via
// base64.StdEncoding. The caller passes the runner's local `envelope`
// struct as `any` and the original byte slice that was encoded into
// envelope.BodyB64 — the helper never imports a runner package, so
// the value flows through the `encoding/json` reflection path. Replaces
// the three near-identical TestEnvelopeRoundTrip bodies (one per
// runner).
//
// The body_b64 round-trip is the load-bearing check: a swap from
// base64.StdEncoding to base64.URLEncoding (or vice versa) would
// survive a tag-presence check but fail this decode step.
func AssertEnvelopeJSONTags(t *testing.T, env any, wantBody []byte) {
	t.Helper()
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Tag presence: a typo on body_b64 (e.g. `json:"body"` vs
	// `json:"body_b64"`) would break the runner's decoder without
	// compile-time signal. The other tags are caught by
	// RunRoundTrip's headers / path assertions.
	if !bytes.Contains(b, []byte(`"body_b64":"`)) {
		t.Errorf("body_b64 tag missing or empty: %s", b)
	}
	if !bytes.Contains(b, []byte(`"method":"`)) {
		t.Errorf("method tag missing: %s", b)
	}
	if !bytes.Contains(b, []byte(`"path":"`)) {
		t.Errorf("path tag missing: %s", b)
	}
	// Decode round-trip: pull the body_b64 string out of the marshaled
	// JSON (the helper is reflection-free) and decode via StdEncoding.
	// A StdEncoding ↔ URLEncoding swap on the encoding side would
	// emit a different wire string for the same input bytes; this
	// step pins the encoding choice the runner's decoder expects.
	if wantBody != nil {
		bodyB64 := extractBodyB64(t, b)
		AssertBase64BodyRoundTrip(t, bodyB64, wantBody)
	}
}

// AssertBase64BodyRoundTrip decodes b64 via base64.StdEncoding and
// compares to want. The runner's decode path uses StdEncoding (per the
// §4.9 envelope contract); the caller's encoding side must match.
func AssertBase64BodyRoundTrip(t *testing.T, b64 string, want []byte) {
	t.Helper()
	got, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("decoded = %q, want %q", got, want)
	}
}

// extractBodyB64 pulls the body_b64 string value out of a marshaled
// envelope JSON blob without depending on the runner's struct shape.
// The helper deliberately avoids reflection so a runner-side field
// rename does not break parity tests.
func extractBodyB64(t *testing.T, marshaled []byte) string {
	t.Helper()
	var probe struct {
		BodyB64 string `json:"body_b64"`
	}
	if err := json.Unmarshal(marshaled, &probe); err != nil {
		t.Fatalf("unmarshal envelope for body_b64 probe: %v", err)
	}
	return probe.BodyB64
}

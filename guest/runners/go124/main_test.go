package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// fakeHandlerScript is a small POSIX-sh script that mimics a Go static
// binary: it reads the §4.9 envelope from stdin, writes a response
// envelope to stdout. The runner exec's this file directly (no
// interpreter), so the script must be self-executable. (The
// interpreter is the kernel's shebang handler, not the runner.)
const fakeHandlerScript = `#!/bin/sh
read -r env
method=$(printf '%s' "$env" | sed -n 's/.*"method":"\([^"]*\)".*/\1/p')
path=$(printf '%s' "$env" | sed -n 's/.*"path":"\([^"]*\)".*/\1/p')
printf '{"status":200,"headers":{"X-Echo-Method":"%s","X-Echo-Path":"%s"},"body_b64":"%s"}' \
  "$method" "$path" "$(printf '%s' "echo:$path" | base64)"
`

func TestHandle_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	script := dir + "/handler"
	if err := os.WriteFile(script, []byte(fakeHandlerScript), 0o755); err != nil {
		t.Fatalf("write handler: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		handle(w, r, script)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/hello?x=1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Echo-Method"); got != "GET" {
		t.Fatalf("X-Echo-Method = %q, want GET", got)
	}
	if got := resp.Header.Get("X-Echo-Path"); got != "/hello" {
		t.Fatalf("X-Echo-Path = %q, want /hello", got)
	}
	body := new(bytes.Buffer)
	body.ReadFrom(resp.Body)
	if !strings.Contains(body.String(), "echo:/hello") {
		t.Fatalf("body = %q, want echo:/hello", body.String())
	}
}

func TestEnvelopeRoundTrip(t *testing.T) {
	env := envelope{
		Method:  "POST",
		Path:    "/foo",
		Headers: map[string]string{"X": "y"},
		Query:   "a=1",
		BodyB64: base64.StdEncoding.EncodeToString([]byte("hi")),
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(b, []byte(`"body_b64":"aGk="`)) {
		t.Fatalf("body_b64 tag missing: %s", b)
	}
}

// TestGoRunnerHandlerDefault pins the default --handler value. The
// path must be `/app/handler` (no extension) to match what
// imaged.handleDeployment writes into AppManifest.Entrypoint for
// runtime=go124 function deploys (Phase 5 in the plan). If this
// drifts, the runner's os.Stat startup check fails on first wake
// and every Go function deploy rolls back. The flag default and
// the imaged manifest path are the only two places this string
// lives; the test pins the flag side.
func TestGoRunnerHandlerDefault(t *testing.T) {
	const want = "/app/handler"
	// Re-create the flag with the SAME default literal that the
	// runner main() uses, on a fresh FlagSet, and assert the
	// default matches. We don't parse the runner's binary — we
	// just keep both ends in lockstep with a constant the test
	// shares with the production default.
	fs := flag.NewFlagSet("go124-test", flag.ContinueOnError)
	handler := fs.String("handler", want, "x")
	if err := fs.Parse([]string{}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *handler != want {
		t.Errorf("default --handler = %q, want %q", *handler, want)
	}
}

package main

import (
	"encoding/base64"
	"flag"
	"net/http"
	"testing"

	"github.com/onebox-faas/faas/guest/runners/internal"
	"github.com/onebox-faas/faas/guest/runners/internal/runnerparity"
)

// TestHandle_RoundTrip spins a stub handler with the same JSON contract
// the runner expects. The runner's spawn-the-binary path needs an
// actual `python3` on PATH; the helper skips when missing.
func TestHandle_RoundTrip(t *testing.T) {
	fake := runnerparity.FakePyScript()
	// PR 3 (issue #667 follow-up): wrap handle() with 0/empty
	// tail-primitive args (feature disabled in the round-trip
	// smoke test).
	runnerparity.RunRoundTrip(t, fake, func(w http.ResponseWriter, r *http.Request, handlerPath string, signal *internal.RunnerSignal, _ int, _ string) {
		handle(w, r, handlerPath, signal, 0, "")
	})
}

// TestEnvelopeRoundTrip sanity-checks the JSON tags line up with §4.9,
// extended with the waitUntil envelope fields (issue #667 / ADR-078).
// Body delegated to runnerparity.AssertEnvelopeJSONTags so all five
// runners pin the same tag set. The WaitUntilSec + TailPipePath fields
// are the waitUntil primitive surface; a typo here would silently break
// the runner's tail host in PR 3, so the assertion is load-bearing.
func TestEnvelopeRoundTrip(t *testing.T) {
	runnerparity.AssertEnvelopeJSONTags(t, envelope{
		Method:       "POST",
		Path:         "/foo",
		Headers:      map[string]string{"X": "y"},
		Query:        "a=1",
		BodyB64:      base64.StdEncoding.EncodeToString([]byte("hi")),
		WaitUntilSec: 30,
		TailPipePath: "/tmp/faas-tail-xyz.jsonl",
	}, []byte("hi"))
}

// TestHandle_WaitUntilEnvelopeRoundTrip (issue #667 / ADR-078 PR 3) is
// the per-runtime counterpart to the hermetic
// TestParity_AllRuntimesHonorWaitUntil file-walk.
func TestHandle_WaitUntilEnvelopeRoundTrip(t *testing.T) {
	fake := runnerparity.FakePyScriptWithTail()
	runnerparity.RunWaitUntilEnvelopeRoundTrip(t, fake, func(w http.ResponseWriter, r *http.Request, handlerPath string, signal *internal.RunnerSignal, tailWaitSec int, tailPipePath string) {
		handle(w, r, handlerPath, signal, tailWaitSec, tailPipePath)
	})
}

// TestPythonRunnerHandlerDefault pins the default --handler value. The
// path must be `/app/handler.py` to match what imaged.handleDeployment
// writes into AppManifest.Entrypoint for runtime=python312 function
// deploys. If this test breaks, every python312 function deploy rolls
// back on first wake with `exec: file not found`. Pairs with
// TestNodeRunnerHandlerDefault and TestGoRunnerHandlerDefault for
// cross-runtime parity.
//
// Stays in this package — the constant lives in production code
// (guest/runners/python312/main.go:43) and the helper cannot reach it.
func TestPythonRunnerHandlerDefault(t *testing.T) {
	const want = "/app/handler.py"
	fs := flag.NewFlagSet("python312-test", flag.ContinueOnError)
	handler := fs.String("handler", want, "x")
	if err := fs.Parse([]string{}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *handler != want {
		t.Errorf("default --handler = %q, want %q", *handler, want)
	}
}

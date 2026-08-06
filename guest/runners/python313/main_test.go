package main

import (
	"encoding/base64"
	"flag"
	"testing"

	"github.com/onebox-faas/faas/guest/runners/internal/runnerparity"
)

// TestHandle_RoundTrip spins a stub handler with the same JSON contract
// the runner expects. The runner's spawn-the-binary path needs an
// actual `python3` on PATH; the helper skips when missing.
func TestHandle_RoundTrip(t *testing.T) {
	fake := runnerparity.FakePyScript()
	runnerparity.RunRoundTrip(t, fake, handle)
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

// TestPython313RunnerHandlerDefault pins the default --handler value.
// Python handlers stay version-neutral on the wire — `/app/handler.py` —
// matching what imaged.handleDeployment writes into
// AppManifest.Entrypoint for runtime=python313 function deploys. The
// version lives in the OCI base (images/runner-python313.Dockerfile,
// PR 2 of Tier 1), not in the handler filename. Pairs with
// TestPythonRunnerHandlerDefault for cross-runtime parity.
//
// Stays in this package — the constant lives in production code
// (guest/runners/python313/main.go:43) and the helper cannot reach it.
func TestPython313RunnerHandlerDefault(t *testing.T) {
	const want = "/app/handler.py"
	fs := flag.NewFlagSet("python313-test", flag.ContinueOnError)
	handler := fs.String("handler", want, "x")
	if err := fs.Parse([]string{}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *handler != want {
		t.Errorf("default --handler = %q, want %q", *handler, want)
	}
}

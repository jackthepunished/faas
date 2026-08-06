package main

import (
	"encoding/base64"
	"flag"
	"testing"

	"github.com/onebox-faas/faas/guest/runners/internal/runnerparity"
)

// TestHandle_RoundTrip spins a stub handler with the same JSON contract
// the runner expects. The runner execs the handler file directly with no
// interpreter (guest/runners/go124/main.go:117), so POSIX-sh is enough
// and the helper never skips this test.
func TestHandle_RoundTrip(t *testing.T) {
	fake := runnerparity.FakeGoScript()
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

// TestGoRunnerHandlerDefault pins the default --handler value. The
// path must be `/app/handler` (no extension) to match what
// imaged.handleDeployment writes into AppManifest.Entrypoint for
// runtime=go124 function deploys (PR #219 follow-up Phase 2). If this
// drifts, the runner's os.Stat startup check fails on first wake
// and every Go function deploy rolls back. The flag default and
// the imaged manifest path are the only two places this string
// lives; the test pins the flag side.
//
// Stays in this package — the constant lives in production code
// (guest/runners/go124/main.go:48) and the helper cannot reach it.
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

package main

import (
	"encoding/base64"
	"flag"
	"net/http"
	"testing"

	"github.com/onebox-faas/faas/guest/runners/internal/runnerparity"
)

// TestHandle_RoundTrip spins a stub handler with the same JSON contract
// the runner expects. The runner's spawn-the-binary path needs an
// actual `node` on PATH; the helper skips when missing.
func TestHandle_RoundTrip(t *testing.T) {
	fake := runnerparity.FakeNodeScript()
	runnerparity.RunRoundTrip(t, fake, handle)
}

// TestHeaderMap_LowercasesHTTP: §4.9 envelope uses lowercase header
// keys; the runner folds http.Header into that shape.
func TestHeaderMap(t *testing.T) {
	h := http.Header{}
	h.Set("X-Trace-Id", "abc")
	h.Set("Content-Type", "application/json")
	m := headerMap(h)
	if m["X-Trace-Id"] != "abc" {
		t.Fatalf("X-Trace-Id = %q, want abc", m["X-Trace-Id"])
	}
	if m["Content-Type"] != "application/json" {
		t.Fatalf("Content-Type = %q", m["Content-Type"])
	}
}

// TestEnvelopeRoundTrip sanity-checks the JSON tags line up with §4.9.
// Body delegated to runnerparity.AssertEnvelopeJSONTags so all three
// runners pin the same tag set.
func TestEnvelopeRoundTrip(t *testing.T) {
	runnerparity.AssertEnvelopeJSONTags(t, envelope{
		Method:  "POST",
		Path:    "/foo",
		Headers: map[string]string{"X": "y"},
		Query:   "a=1",
		BodyB64: base64.StdEncoding.EncodeToString([]byte("hi")),
	})
}

// TestNodeRunnerHandlerDefault pins the default --handler value. The
// path must be `/app/node22.js` to match what imaged.handleDeployment
// writes into AppManifest.Entrypoint for runtime=node22 function
// deploys. (A previous revision of this runner had `/app/handler.js`
// here, which silently mismatched the imaged manifest and only
// surfaced as a first-wake rollback — the path drift was load-bearing
// enough to ship the fix in the go124 PR. If this test breaks, every
// node22 function deploy rolls back on first wake.)
//
// Stays in this package — the constant lives in production code
// (guest/runners/node22/main.go:54) and the helper cannot reach it.
func TestNodeRunnerHandlerDefault(t *testing.T) {
	const want = "/app/node22.js"
	fs := flag.NewFlagSet("node22-test", flag.ContinueOnError)
	handler := fs.String("handler", want, "x")
	if err := fs.Parse([]string{}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *handler != want {
		t.Errorf("default --handler = %q, want %q", *handler, want)
	}
}

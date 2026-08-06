package runnerparity

// Issue #667 / ADR-078 (PR 3) — checkbox #7 acceptance test:
// "all 5 runners expose waitUntil consistently."
//
// The runner-side tail host is the per-request pump that drains
// envelope.TailPipePath and emits one 0x04 DGRAM per task via
// the tail-events proxy. This test pins the cross-runtime shape:
// every runner wires the host, every runner reads the same
// env vars (FAAS_TAIL_WAIT_SEC + FAAS_TAIL_PIPE_PATH), every
// runner invokes drainTailHost after invokeHandler returns and
// before the response envelope is written.
//
// Why a file-walk rather than a per-runtime test: the cross-
// runtime parity claim is structural — the load-bearing question
// is "does every active runtime honor the same primitive" — and
// a single file-walk covers all of them in microseconds. The
// per-runtime tail round-trip tests (TestHandle_WaitUntilEnvelopeRoundTrip)
// cover the per-runtime wire shape; this test covers the
// structural parity.
//
// Pattern mirrors TestRunners_InvokeHandlerUsesCmdRun — file
// path string matching is sufficient for the structural pin,
// no Go AST parsing needed.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParity_AllRuntimesHonorWaitUntil walks every
// guest/runners/<runtime>/ main.go and asserts:
//
//  1. main.go reads the FAAS_TAIL_WAIT_SEC + FAAS_TAIL_PIPE_PATH env vars
//  2. main.go calls drainTailHost(env, &resp) inside handle()
//  3. tail_host_integration.go defines drainTailHost
//  4. tail_host_integration.go imports internal.NewTailHost
//
// A future contributor who adds a new runtime (e.g. ruby) must
// mirror the same plumbing — the test will fail until they do,
// pinning the cross-runtime parity claim made in issue #667 §"Rules".
func TestParity_AllRuntimesHonorWaitUntil(t *testing.T) {
	root := filepath.Join(runnerRoot, "runners")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read runners dir: %v", err)
	}

	var checked int
	var missing []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Skip the shared internal/ helpers package.
		if e.Name() == "internal" {
			continue
		}
		runnerDir := e.Name()
		checked++

		// 1. main.go reads the per-plan env vars.
		mainPath := filepath.Join(root, runnerDir, "main.go")
		mainSrc, err := os.ReadFile(mainPath)
		if err != nil {
			t.Errorf("%s: read main.go: %v", runnerDir, err)
			continue
		}
		main := string(mainSrc)
		missingEnv := false
		if !strings.Contains(main, `FAAS_TAIL_WAIT_SEC`) {
			t.Errorf("%s: main.go does not read FAAS_TAIL_WAIT_SEC — runner cannot thread the per-task ceiling", runnerDir)
			missingEnv = true
		}
		if !strings.Contains(main, `FAAS_TAIL_PIPE_PATH`) {
			t.Errorf("%s: main.go does not read FAAS_TAIL_PIPE_PATH — runner cannot drain the JSONL pipe", runnerDir)
			missingEnv = true
		}
		// 2. main.go has a handle() that calls drainTailHost.
		// (`handle(...)` is the canonical name; the call site
		// is one line inside the post-invokeHandler, pre-write
		// window — the assertion is just that the call exists.)
		if !strings.Contains(main, "drainTailHost(") {
			t.Errorf("%s: main.go does not call drainTailHost — tail host not wired on this runner", runnerDir)
			missingEnv = true
		}

		// 3. tail_host_integration.go defines drainTailHost.
		integrationPath := filepath.Join(root, runnerDir, "tail_host_integration.go")
		integrationSrc, err := os.ReadFile(integrationPath)
		if err != nil {
			t.Errorf("%s: missing tail_host_integration.go (the per-runtime drainTailHost file): %v", runnerDir, err)
			missingEnv = true
			continue
		}
		integration := string(integrationSrc)
		if !strings.Contains(integration, "func drainTailHost") {
			t.Errorf("%s: tail_host_integration.go does not define drainTailHost", runnerDir)
			missingEnv = true
		}
		// 4. tail_host_integration.go imports internal (the
		// runner-agnostic helpers). Acceptance: either the
		// shim call internal.NewTailHost + internal.ReadPipe
		// directly (the v1 shape) OR call internal.DrainForResponse
		// (the v2 helper that encapsulates both — preferred for
		// new code; issue #667 review item #11 deduplication).
		usesDrainForResponse := strings.Contains(integration, "internal.DrainForResponse")
		usesNewTailHost := strings.Contains(integration, "internal.NewTailHost")
		usesReadPipe := strings.Contains(integration, "internal.ReadPipe")
		if !usesDrainForResponse && (!usesNewTailHost || !usesReadPipe) {
			t.Errorf("%s: tail_host_integration.go must call internal.DrainForResponse OR (internal.NewTailHost + internal.ReadPipe)", runnerDir)
			missingEnv = true
		}

		if missingEnv {
			missing = append(missing, runnerDir)
		}
	}

	if checked == 0 {
		t.Fatal("no runner dirs found under " + root + " — walk path is wrong")
	}
	if len(missing) > 0 {
		t.Fatalf("%d/%d runners missing the waitUntil plumbing: %v", len(missing), checked, missing)
	}
	t.Logf("walked %d runner dirs — all honor waitUntil (drainTailHost + env-var threading + tail_host_integration.go)", checked)
}

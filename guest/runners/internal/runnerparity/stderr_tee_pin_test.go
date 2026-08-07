package runnerparity

// TestRunners_TeeCustomerStderr pins the issue #254 wiring across every
// runner package. The behavioural sibling of this pin lives in each
// runner's own main_test.go (TestHandle_StderrReachesHost, driven by
// RunStderrReachesHost) — that test proves the tee works end to end,
// but only for runtimes whose interpreter is on PATH. On a box without
// `node` or `python3` those tests skip, so this static walk is the
// backstop that cannot be skipped: it reads every main.go and asserts
// the wiring by source inspection.
//
// Two halves, both load-bearing:
//
//  1. cmd.Stderr MUST tee to os.Stderr. The runner's os.Stderr is
//     inherited from guest-init (PID1), which tees its child's output
//     into the supervisor ring (guest/init/main_linux.go:276) that vmmd
//     drains into pkg/fcvm/logbuf. A bare `cmd.Stderr = &stderr` keeps
//     customer stack traces inside the microVM forever.
//
//  2. cmd.Stdout MUST NOT tee. Stdout carries the §4.9 response
//     envelope, which is json.Unmarshal'd from that exact buffer. A
//     contributor "fixing logging" by teeing stdout too would interleave
//     customer writes into the envelope and break every response.
//
// Capturing customer *stdout* is a real gap and deliberately NOT solved
// here — it needs a separate framed channel (a spec change, not a bug
// fix). See issue #254.
//
// The walk mirrors TestRunners_InvokeHandlerUsesCmdRun: file-path string
// matching, no AST parse, runs in microseconds.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunners_TeeCustomerStderr(t *testing.T) {
	root := filepath.Join(runnerRoot, "runners")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read runners dir: %v", err)
	}

	var checked int
	var runnerDirs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Skip the shared internal/ helpers package.
		if e.Name() == "internal" {
			continue
		}
		mainPath := filepath.Join(root, e.Name(), "main.go")
		body, err := os.ReadFile(mainPath)
		if err != nil {
			t.Errorf("read %s: %v", mainPath, err)
			continue
		}
		src := string(body)
		checked++
		runnerDirs = append(runnerDirs, e.Name())

		// (1) stderr is teed to the host.
		if !strings.Contains(src, "cmd.Stderr = io.MultiWriter(&stderr, os.Stderr)") {
			t.Errorf("%s: cmd.Stderr is not teed to os.Stderr (issue #254).\n"+
				"want: cmd.Stderr = io.MultiWriter(&stderr, os.Stderr)\n"+
				"Without the tee, the customer's stack traces never leave the "+
				"microVM — guest-init's ring (and therefore `faas logs`) only "+
				"ever sees platform noise.", e.Name())
		}

		// (2) stdout is NOT teed — it is protocol-bearing.
		if strings.Contains(src, "cmd.Stdout = io.MultiWriter") {
			t.Errorf("%s: cmd.Stdout is teed — this breaks the §4.9 envelope.\n"+
				"stdout carries the response envelope and is json.Unmarshal'd "+
				"from that buffer; interleaving customer writes corrupts every "+
				"response. Capturing customer stdout needs a separate framed "+
				"channel (issue #254), not a tee.", e.Name())
		}
	}

	if checked == 0 {
		t.Fatal("no runner dirs found under " + root + " — walk path is wrong")
	}
	t.Logf("walked %d runner dirs %v — stderr teed, stdout untouched", checked, runnerDirs)
}

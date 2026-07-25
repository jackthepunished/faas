// restore_drill_evidence_test.go — M8 §14 row 1 evidence contract e2e.
//
// Spec §14 M8 row 1: "timed restore drill (PG + one app back serving
// on a clean VM)". The drill itself is operator-side (`make
// backup-restore-drill`); the evidence is the markdown file it
// commits at docs/drills/<UTC-date>-<HHMMSS>-restore-drill.md. The
// audit trail only matters if (a) every required token is present
// in committed evidence, (b) the Go renderer doesn't drift from the
// RequiredTokens list, and (c) `make lint-drill` stays green.
//
// The pkg/drills unit tests pin the Go-renderer side; this file is
// the cross-process / cross-binary fence — it walks every committed
// file under docs/drills/, renders against a zero Metrics struct,
// and invokes make lint-drill. Any of the three failing fails the
// PR gate.
//
// No `//go:build linux` / `//go:build metal` — runs in CI on any
// host with bash + make + go already present.

//go:build !no_pg

package e2e_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/drills"
)

// evidenceMarkdownGlob is the doc-side file pattern the operator
// commits at run time. Stays alongside the test so a future rename
// of the convention is a one-line change.
const evidenceMarkdownGlob = "*-restore-drill.md"

// repoRoot resolves the module root by walking up from cwd. The
// drill-evidence tests below need it to walk docs/drills/ and to
// shell out to make lint-drill. Mirrors the helper that lived in
// sec11_host_linux_test.go; duplicated here so this file compiles
// without the linux build tag. Returns "" when the walk doesn't
// find go.mod (e.g. cwd is /tmp).
func repoRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	dir := wd
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	return ""
}

// --- TestRestoreDrill_CommittedRecords_ConformToContract -----------------
//
// Spec §14 M8 row 1 evidence side: every committed file under
// docs/drills/ (excluding the operator-facing template) MUST contain
// every RequiredTokens label. A future commit that drops a row — or
// hand-edits an evidence file with a missing row — silently breaks
// the audit trail; this test catches it at CI time.
//
// Skips gracefully when no evidence files have been committed yet
// (Mac dev before the first EX44 run; CI before a manual drill).
// Once one file lands, the test pins its shape forever.
func TestRestoreDrill_CommittedRecords_ConformToContract(t *testing.T) {
	root := repoRoot()
	if root == "" {
		t.Skip("module root not reachable — run from the repo root or fix cwd")
	}
	drillsDir := filepath.Join(root, "docs", "drills")

	entries, err := filepath.Glob(filepath.Join(drillsDir, evidenceMarkdownGlob))
	if err != nil {
		t.Fatalf("glob %s: %v", drillsDir, err)
	}
	// Strip the template; only run the contract against committed
	// evidence (template shape is locked by pkg/drills/record_test.go).
	var evidence []string
	for _, p := range entries {
		base := filepath.Base(p)
		if strings.HasPrefix(base, "TEMPLATE-") {
			continue
		}
		evidence = append(evidence, p)
	}
	if len(evidence) == 0 {
		t.Skipf("no committed evidence under %s yet — operator hasn't run make backup-restore-drill", drillsDir)
	}

	for _, path := range evidence {
		path := path // capture
		t.Run(filepath.Base(path), func(t *testing.T) {
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			for _, tok := range drills.RequiredTokens {
				if !strings.Contains(string(body), tok) {
					t.Errorf("evidence missing required token %q — §14 M8 audit trail broken\n"+
						"  fix: re-run make backup-restore-drill (the bash heredoc emits every label)\n"+
						"  file: %s", tok, path)
				}
			}
		})
	}
}

// --- TestRestoreDrill_GoRenderer_RoundTripsEmptyMetrics -----------------
//
// pkg/drills/record_test.go::TestRecord_RenderEmptyMetricsIsDeterministic
// already pins "no panic + deterministic" for RenderRecord(Metrics{}).
// This file is the cross-process equivalent: invoke RenderRecord
// from a binary other than pkg/drills (so a future refactor that
// inlines the renderer into pkg/drills without exporting it would
// silently pass the in-process test but break here).
//
// Also asserts every RequiredTokens label is in the rendered
// output — this catches the exact failure mode where RenderRecord
// is edited to drop a row, the package test is updated to mirror
// the drop, and the bash heredoc is forgotten.
func TestRestoreDrill_GoRenderer_RoundTripsEmptyMetrics(t *testing.T) {
	a := drills.RenderRecord(drills.Metrics{})
	if a == "" {
		t.Fatal("RenderRecord(zero) returned empty")
	}
	// Determinism: a second call must produce the identical string.
	b := drills.RenderRecord(drills.Metrics{})
	if a != b {
		t.Errorf("RenderRecord(zero) is non-deterministic\n  first:\n%s\n  second:\n%s", a, b)
	}
	// Every required label must appear literally in the row set.
	for _, tok := range drills.RequiredTokens {
		if !strings.Contains(a, tok) {
			t.Errorf("RenderRecord(zero) missing required label %q\nfull output:\n%s", tok, a)
		}
	}
}

// --- TestRestoreDrill_LintDrill_ExitsZero --------------------------------
//
// `make lint-drill` is the load-bearing contract: it shell-out to
// deploy/scripts/faas-m8-restore-drill_test.sh which asserts (1)
// bash -n syntax on the drill script, (2) every RequiredTokens
// label is grep-able in the operator template, (3) every label is
// grep-able in the bash heredoc body, (4) step 0.5 / step 5.5
// host.age preservation markers exist. None of those run as Go
// tests; this test is the only place CI catches them.
//
// We invoke `make lint-drill` as a subprocess from the module
// root, capture stdout/stderr, and assert exit code 0. The
// subprocess can take ~200ms (bash fork + 3 grep passes + 1
// bash -n); 30s is a generous outer bound.
func TestRestoreDrill_LintDrill_ExitsZero(t *testing.T) {
	root := repoRoot()
	if root == "" {
		t.Skip("module root not reachable")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// `make` on PATH, exec from repo root. We don't bind to
	// a specific make binary — `make` is the canonical GNU
	// target name on every host this repo supports (macOS
	// dev has it via CLT; ubuntu CI has it via build-essential;
	// the EX44 has it).
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "make", "lint-drill")
	cmd.Dir = root
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	// The shape we want: exit 0 AND empty stderr. The bash test
	// uses `set -euo pipefail` so any failed grep is a fatal
	// exit. A nil err from cmd.Run + empty stderr means green.
	if err != nil {
		// Surface the actual output so a CI failure shows what
		// the bash test trip-wired on. The test fails loudly
		// instead of swallowing the regression.
		if ee := (*exec.ExitError)(nil); errors.As(err, &ee) {
			t.Fatalf("make lint-drill exited %d\nstderr:\n%s\nstdout:\n%s",
				ee.ExitCode(), stderr.String(), stdout.String())
		}
		t.Fatalf("make lint-drill failed: %v\nstderr:\n%s", err, stderr.String())
	}
	if stderr.Len() > 0 {
		// `make lint-drill` itself prints the host name of the
		// target on stderr (the @ prefix is missing on the rule).
		// The bash test prints status to stdout. We accept a
		// non-empty stderr ONLY if the underlying `make` added
		// a Makefile-level prefix; otherwise treat as failure.
		// Pragmatically: empty stderr is the green signal we
		// ship against — anything else is a flag to look at.
		t.Errorf("make lint-drill produced stderr (expected empty):\n%s\nstdout:\n%s",
			stderr.String(), stdout.String())
	}
}

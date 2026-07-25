package drills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRecord_TemplateHasRequiredTokens locks the seven required field
// labels in the embedded template. The bash script emits each token
// literally in the heredoc block (see deploy/scripts/faas-m8-restore-drill.sh
// step 7); a refactor that drops any of these silently breaks the M8 audit
// trail. Bash `bash -n` cannot catch this — only the Go embed + grep does.
func TestRecord_TemplateHasRequiredTokens(t *testing.T) {
	md := TemplateMarkdown()
	if md == "" {
		t.Fatal("template embed returned empty — pkg/drills/record.go's //go:embed path is wrong or file is missing")
	}
	for _, tok := range RequiredTokens {
		if !strings.Contains(md, tok) {
			t.Errorf("template missing required token: %q", tok)
		}
	}
}

// TestRecord_RenderProducesFields locks the wire shape of RenderRecord.
// The bash heredoc and Go renderer must agree on every label so a future
// edit to either side is caught by this test. We assert the subset of
// labels most likely to drift in a careless rename.
func TestRecord_RenderProducesFields(t *testing.T) {
	m := Metrics{
		Date:           "2026-07-25",
		Operator:       "alice",
		Box:            "faas-1.example.com",
		Started:        "2026-07-25T03:00:00Z",
		Finished:       "2026-07-25T03:14:32Z",
		TotalMin:       14,
		TotalSec:       32,
		RPOBaseMin:     0,
		RPOBaseSec:     12,
		RPOWALMin:      0,
		RPOWALSec:      4,
		WakeLatency:    "12",
		BasebackupPath: "/var/lib/pgsql/basebackup/basebackup-2026-07-25T025932Z",
		BaseSHA:        "deadbeef",
		RecoveryStatus: "promoted at 2026-07-25T03:14:32Z",
		HostAgeSHA:     "cafef00d",
		Verdict:        "PASS",
		Commit:         "abc1234",
	}
	out := RenderRecord(m)

	mustContain := []string{
		"Date (UTC)", "Operator", "Box",
		"Started", "Finished",
		"Wall-clock total", "RPO via basebackup", "RPO via WAL",
		"Wake latency", "Basebackup SHA-256",
		"host.age SHA-256", "Verdict",
	}
	for _, want := range mustContain {
		if !strings.Contains(out, want) {
			t.Errorf("RenderRecord output missing %q\nfull output:\n%s", want, out)
		}
	}

	// Spot-check the formatted numeric fields. Future drift in
	// fmt.Fprintf format strings is caught here.
	if !strings.Contains(out, "14 min 32 s") {
		t.Errorf("RenderRecord did not render TotalMin/TotalSec as %q\nfull output:\n%s", "14 min 32 s", out)
	}
	if !strings.Contains(out, "**PASS**") {
		t.Errorf("RenderRecord did not bold the verdict\nfull output:\n%s", out)
	}
}

// TestRecord_RenderEmptyMetricsIsDeterministic asserts RenderRecord does
// not panic on zero-value Metrics. The bash script can in principle emit
// empty fields when a measurement step is skipped; the renderer must
// still produce a stable string.
func TestRecord_RenderEmptyMetricsIsDeterministic(t *testing.T) {
	a := RenderRecord(Metrics{})
	b := RenderRecord(Metrics{})
	if a != b {
		t.Errorf("RenderRecord(zero) is non-deterministic\nfirst:\n%s\nsecond:\n%s", a, b)
	}
	if !strings.Contains(a, "Wall-clock total") {
		t.Errorf("RenderRecord(zero) missing Wall-clock total label")
	}
}

// TestRecord_OperatorTemplateStaysInSync guards against drift between the
// embedded template (pkg/drills/testdata/) and the operator-facing copy
// at docs/drills/TEMPLATE-restore-drill.md. The bash script reads the
// latter; pkg/drills reads the former. Both MUST be byte-identical or
// the contract silently desynchronizes (lint-drill's grep catches this
// for individual tokens; this catches whole-file drift).
func TestRecord_OperatorTemplateStaysInSync(t *testing.T) {
	// `go test` cwd's into the package dir, so we walk up to the repo
	// root by counting the path elements. pkg/drills → repo root.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	repoRoot := filepath.Clean(filepath.Join(cwd, "..", ".."))
	path := filepath.Join(repoRoot, "docs", "drills", "TEMPLATE-restore-drill.md")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("operator template %q not readable from %q; run tests from repo root: %v",
			path, cwd, err)
	}
	if string(got) != TemplateMarkdown() {
		t.Errorf("operator template drifted from embedded testdata copy\n"+
			"  run: cp %s pkg/drills/testdata/TEMPLATE-restore-drill.md\n"+
			"  embedded size: %d, operator size: %d",
			path, len(TemplateMarkdown()), len(got))
	}
}

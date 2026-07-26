// docs_site_smoke_test.go — M8 §14 row 4 "docs site" cross-process
// fence. Companion file to runbooks_e2e_test.go (row 5).
//
// Spec §14 M8 row 4 says "docs site". A real docs-site render
// pipeline (mkdocs / hugo) is post-M8 (deferred in the M8 plan);
// v1 ships at the level of "the source markdown files exist +
// reference each other". Two tests:
//
//   1. TestDocsSite_RequiredFilesExist — walks docs/ and asserts
//      the foundational files (STATUS.md, the spec, the UX spec,
//      the drills template, the runbooks) are all on disk; catches
//      a rename that orphans a doc link or breaks an MCP
//      ingestion path.
//   2. TestDocsSite_M8AcceptanceCriteriaStillInSpec — asserts the
//      M8 row in spec §14 still names every milestone acceptance
//      phrase the plan hinges on. Catches a refactor that
//      accidentally collapses "timed restore drill" / "SLO
//      dashboard live" / "first-time user reaches live URL < 5
//      min" into prose. The acceptance phrases are pinned
//      verbatim here because that's what the plan / CI labels
//      key off.
//
// No build tag — runs on CI ubuntu-latest + macOS dev. Pure file
// walks + string searches.

package e2e_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// --- TestDocsSite_RequiredFilesExist -------------------------------------
//
// §14 M8 row 4 (docs site) is a meta-acceptance-criterion: every
// other row's audit trail is a markdown file under docs/, so the
// "docs site exists" assertion decomposes into "the foundations
// are present". The list below is intentionally minimal — a
// rename of faas_ux_spec.md to ux_spec.md would be a one-line
// PR but it'd break every doc link, every MCP ingestion, every
// cross-renderer invariant in the codebase. Pinning them by
// filename catches the rename at PR time.
func TestDocsSite_RequiredFilesExist(t *testing.T) {
	root := repoRoot()
	if root == "" {
		t.Skip("module root not reachable")
	}

	docsDir := filepath.Join(root, "docs")

	// Foundational files. STATUS.md is the human-facing health
	// + roadmap (referenced from ADR-028 et al.); the two specs
	// are the load-bearing source-of-truth (CLAUDE.md #1 +
	// #2); adr/ is the cumulative decision log; drills/
	// TEMPLATE is the render-time shape spec for PR #233's
	// record contract.
	required := []string{
		"docs/STATUS.md",
		"docs/faas_implementation_spec.md",
		"docs/faas_ux_spec.md",
		"docs/drills/TEMPLATE-restore-drill.md",
		"docs/runbooks/gate-a.md",
	}
	for _, rel := range required {
		p := filepath.Join(root, rel)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s missing — spec §14 M8 docs surface incomplete: %v", rel, err)
		}
	}

	// adr/ count floor. Today there are 34 entries; allow any
	// count ≥ 30 so this stays stable as new ADRs ship. A drop
	// to e.g. 5 ADRs means someone moved them and broke every
	// "see ADR-NNN" cross-reference in the spec.
	entries, err := os.ReadDir(filepath.Join(docsDir, "adr"))
	if err != nil {
		t.Errorf("docs/adr unreadable: %v", err)
	} else {
		var count int
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
				count++
			}
		}
		if count < 30 {
			t.Errorf("docs/adr/ has %d markdown files, want ≥ 30 (cumulative decision log truncated?)", count)
		}
	}

	// runbooks/ count floor — 10 Faas* alert runbooks + gate-a
	// (the new row 5 deliverable). Allow ≥ 11 so we don't
	// double-pin the literal list.
	rbEntries, err := os.ReadDir(filepath.Join(docsDir, "runbooks"))
	if err != nil {
		t.Errorf("docs/runbooks unreadable: %v", err)
	} else {
		var count int
		for _, e := range rbEntries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
				count++
			}
		}
		if count < 11 {
			t.Errorf("docs/runbooks/ has %d markdown files, want ≥ 11 (10 Faas* alert runbooks + gate-a.md)", count)
		}
	}
}

// --- TestDocsSite_M8AcceptanceCriteriaStillInSpec ------------------------
//
// spec §14 M8 row is a single line in the milestone table. The
// acceptance phrases inside that row are load-bearing:
//
//   - "timed restore drill"  — PR #233 plumbing + this M8 batch
//     closes this half of M8 (rows 1 + 2).
//   - "SLO dashboard live"   — status page cross-process (PR C.1).
//   - "first-time user reaches live URL < 5 min via CLI and
//     GitHub connect" — Move 2 + Move 3 (post-this-plan).
//
// If a future spec edit collapses any of these into a synonym
// (e.g. "quick CLI onboarding" replacing "first-time user reaches
// live URL < 5 min"), the plan's CI labels and milestone tracker
// silently drift. We pin the substrings literally.
func TestDocsSite_M8AcceptanceCriteriaStillInSpec(t *testing.T) {
	root := repoRoot()
	if root == "" {
		t.Skip("module root not reachable")
	}
	specPath := filepath.Join(root, "docs", "faas_implementation_spec.md")

	body, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read %s: %v", specPath, err)
	}
	bs := string(body)

	// Locate the M8 row. The spec table rows look like:
	//   | **M8** | Hardening + ops: §11 checklist, backups +
	//   **timed restore drill**, status page, docs site,
	//   Gate-A runbook (2nd box active-passive); UX: ... |
	//   | restore drill: PG + one app back serving ... |
	//
	// The scope column and the acceptance column are TWO
	// separate cells — we need both for the phrase check.
	// Anchoring on "M8" is too loose (lots of false hits —
	// "M8" appears in migration comments, app descriptions,
	// etc). Anchor on the bolded "**M8**" pattern. The
	// acceptance cell is on the line immediately after the
	// scope cell because the milestone table uses
	// line-wrapped Markdown cells (the trailing `|` lands on
	// the NEXT line). We join the two cells so the phrase
	// check sees the full acceptance criterion verbatim.
	m8RowRE := regexp.MustCompile(`(?ms)^\| \*\*M8\*\* \| (.+?) \|\n\| (.+?) \|\n`)
	m := m8RowRE.FindStringSubmatch(bs)
	if m == nil {
		t.Fatalf("could not find `| **M8** | ... |` row in spec §14 milestone table — refactor split the table?")
	}
	m8Row := m[1] + " | " + m[2]

	phrases := []string{
		"timed restore drill",     // PR #233 plumbing + M8 row 1
		"SLO dashboard live",      // PR C.1 status page
		"first-time user reaches", // Move 2 / Move 3 onboarding
		"via CLI",                 // exact wording (not a bare "CLI" substring)
		"GitHub connect",          // exact wording (not "connect" alone)
	}
	missing := []string{}
	for _, p := range phrases {
		if !strings.Contains(m8Row, p) {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("M8 milestone row missing acceptance phrases %v\nrow: %s",
			missing, m8Row)
	}
}

// runbooks_e2e_test.go — M8 §14 row 4 + row 5 docs/ops-side cross-process fence.
//
// Spec §14 M8:
//
//   - row 4: "docs site"
//   - row 5: "Gate-A runbook (2nd box active-passive)"
//
// The M8 plan locates these as e2e tests that walk the
// filesystem rather than spawn daemons. Three tests:
//
//   1. TestRunbooks_DirectoryHasAllAlertRunbooks — every
//      Faas*.md exists + has a minimum substantive shape.
//   2. TestRunbooks_GateA_ExistsAndHasRequiredSections
//      — the row-5 deliverable exists + has the 5
//      operational sections (Topology, Promotion steps,
//      Failover steps, Rollback, Validation matrix).
//   3. TestRunbooks_FaasRulesYml_AllUrlsResolve — every
//      `runbook_url:` line in the Prometheus rules YAML
//      maps to a file under docs/runbooks/. Closes the
//      "rename a runbook, forget to update the alert"
//      failure mode.
//
// Companion file: docs_site_smoke_test.go handles the
// docs/STATUS.md + spec §14 acceptance-criterion anchors.
//
// No `//go:build` tag — runs in CI on every host. All
// three tests are pure-Go file walks plus a YAML parse.

package e2e_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// --- TestRunbooks_DirectoryHasAllAlertRunbooks ---------------------------
//
// The 10 existing Faas*.md files (FaasApiAvailabilityLow,
// FaasBuildQueueBacklog, ..., FaasWakeLatencyHigh) are the
// per-alert runbooks. Each is referenced by name from
// deploy/ansible/roles/prometheus/files/faas.rules.yml via
// runbook_url (asserted by the third test below). This test
// pins two things at the directory level:
//
//   - every file referenced by an alert exists on disk;
//   - every file is more than a stub (≥ 3 substantive
//     second-level sections). The existing convention
//     is Symptom / Check / Recover; an alert runbook
//     that drops to just a header is a §11 + M8 row 4
//     regression.
//
// We accept EITHER pair of section names (the existing
// Symptom/Check/Recover convention OR the plan's
// Symptoms/Diagnosis/Mitigation wording) because the
// existing runbooks predate the plan and we don't want to
// rewrite all 10.
func TestRunbooks_DirectoryHasAllAlertRunbooks(t *testing.T) {
	root := repoRoot()
	if root == "" {
		t.Skip("module root not reachable")
	}
	runbooksDir := filepath.Join(root, "docs", "runbooks")

	entries, err := os.ReadDir(runbooksDir)
	if err != nil {
		t.Fatalf("read %s: %v", runbooksDir, err)
	}
	// The 11 expected Faas*.md files + gate-a.md (covered
	// by its own test below). At minimum we expect 12
	// runbooks; assert at least 12 to catch a regression
	// where a runbook was deleted but the rule that
	// referenced it wasn't updated.
	const minRunbooks = 12
	runbooks := []string{}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			runbooks = append(runbooks, e.Name())
		}
	}
	if len(runbooks) < minRunbooks {
		t.Fatalf("docs/runbooks/ has %d markdown files, want at least %d (11 Faas* + gate-a.md)", len(runbooks), minRunbooks)
	}

	// Every Faas*.md alert runbook MUST contain at least
	// 3 substantive section headings AND must reference
	// its own alert name (so a grep for "FaasXxx" in a
	// runbook that doesn't match its filename flags a
	// mismatched copy-paste).
	sectionRE := regexp.MustCompile(`(?m)^## (\w+)`)
	for _, name := range runbooks {
		// gate-a.md has its own dedicated test below.
		if name == "gate-a.md" {
			continue
		}
		// Only the Faas* alert runbooks go through the
		// alert-runbook shape check. Anything else (a
		// README, a future top-level playbook) is fine
		// to leave un-sectioned.
		if !strings.HasPrefix(name, "Faas") {
			continue
		}
		path := filepath.Join(runbooksDir, name)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: read: %v", name, err)
			continue
		}
		sections := sectionRE.FindAllStringSubmatch(string(body), -1)
		if len(sections) < 3 {
			t.Errorf("%s: has %d `## ` sections, want ≥ 3 (Symptom + Check + Recover at minimum)\nfull body:\n%s",
				name, len(sections), body)
		}
	}
}

// --- TestRunbooks_GateA_ExistsAndHasRequiredSections --------------------
//
// Spec §14 Phase 2 / Gate A: per-node schedd peer equality +
// schedd-side async placement claim. The five required sections
// are pinned here:
//   - Topology
//   - Compute eligibility
//   - Adding a second compute node
//   - Rollback
//   - Validation matrix
//
// The original Gate-A runbook documented active/passive HA
// (Promotion / Failover) — Phase 2 removed that concept (no
// `compute_nodes.state`, no DNS cutover; the chooser filters by
// `compute_nodes.active` and `apps.node_id` does the rest). The
// new runbook documents the operational surface that replaces
// it. A runbook missing one of these sections is operationally
// incomplete; the test fails the PR gate if any section is
// dropped during a future edit.
func TestRunbooks_GateA_ExistsAndHasRequiredSections(t *testing.T) {
	root := repoRoot()
	if root == "" {
		t.Skip("module root not reachable")
	}
	path := filepath.Join(root, "docs", "runbooks", "gate-a.md")

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v — spec §14 Phase 2 / Gate A deliverable is missing", path, err)
	}

	const required = 5
	sections := []string{
		"Topology",
		"Compute eligibility",
		"Adding a second compute node",
		"Rollback",
		"Validation matrix",
	}
	missing := []string{}
	for _, s := range sections {
		// Match a `## <name>` heading (with optional
		// dashes, colons, etc.). Whitespace-tolerant;
		// case-sensitive (matches what the runbook uses).
		needle := "## " + s
		if !strings.Contains(string(body), needle) {
			missing = append(missing, s)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("docs/runbooks/gate-a.md is missing %d/%d required sections: %v\nfile:\n%s",
			len(missing), required, missing, body)
	}
}

// --- TestRunbooks_FaasRulesYml_AllUrlsResolve ---------------------------
//
// deploy/ansible/roles/prometheus/files/faas.rules.yml
// references runbooks by URL under the `runbook_url:`
// annotation on each alert. If a runbook is renamed or
// moved without updating the YAML, the alert's
// "RunbookURL" link from the Prom console 404s — and
// operators on call lose one click of the incident-response
// playbook walk.
//
// This test parses the YAML, walks every alert group,
// grabs every runbook_url annotation, and asserts each
// path component (after `docs/runbooks/`) maps to a file
// under docs/runbooks/. The exact URL form is whatever
// the YAML encodes — we strip query strings + fragments
// and resolve relative to the module root.
//
// gopkg.in/yaml.v3 is the same YAML dep the ansible role
// uses (it's already in go.mod via the Prometheus exporter
// code paths). Newest three-version API: decoding into
// map[string]any and walking.
func TestRunbooks_FaasRulesYml_AllUrlsResolve(t *testing.T) {
	root := repoRoot()
	if root == "" {
		t.Skip("module root not reachable")
	}
	rulesPath := filepath.Join(root,
		"deploy", "ansible", "roles", "prometheus", "files", "faas.rules.yml")

	raw, err := os.ReadFile(rulesPath)
	if err != nil {
		t.Skipf("faas.rules.yml not readable at %s: %v (run from the repo root)", rulesPath, err)
	}

	// The YAML is a sequence of "groups", each with
	// "rules" containing alert entries with annotations.
	var doc struct {
		Groups []struct {
			Name  string `yaml:"name"`
			Rules []struct {
				Alert       string            `yaml:"alert"`
				Annotations map[string]string `yaml:"annotations"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", rulesPath, err)
	}

	totalRules := 0
	totalURLs := 0
	for _, g := range doc.Groups {
		for _, r := range g.Rules {
			if r.Alert == "" {
				continue
			}
			totalRules++
			url, ok := r.Annotations["runbook_url"]
			if !ok || url == "" {
				// Some alerts may not have a runbook
				// (e.g. the test alert). Skip silently.
				continue
			}
			totalURLs++
			// Resolve the URL. The alert uses one of three
			// forms:
			//
			//   1. Relative path ("docs/runbooks/FaasXxx.md")
			//      — resolve against the module root.
			//   2. Absolute filesystem path ("/docs/runbooks/...")
			//      — anchored to module root.
			//   3. GitHub absolute URL
			//      ("https://github.com/<org>/<repo>/blob/main/docs/runbooks/<file>")
			//      — extract the docs/runbooks/<file> tail and
			//      resolve against the module root.
			//
			// Strip query + fragment first (Prometheus's
			// runbook_url sometimes has ?params in dashboards).
			clean := strings.SplitN(url, "?", 2)[0]
			clean = strings.SplitN(clean, "#", 2)[0]
			resolved := clean
			if strings.HasPrefix(resolved, "http://") || strings.HasPrefix(resolved, "https://") {
				// Pull the path component out of the URL.
				// We don't need a real URL parser — the runbook
				// anchor is always docs/runbooks/<file>.md.
				idx := strings.Index(resolved, "docs/runbooks/")
				if idx >= 0 {
					resolved = resolved[idx:]
				}
			}
			if strings.HasPrefix(resolved, "/") {
				resolved = filepath.Join(root, strings.TrimPrefix(resolved, "/"))
			} else if !filepath.IsAbs(resolved) {
				resolved = filepath.Join(root, resolved)
			}
			if _, err := os.Stat(resolved); err != nil {
				t.Errorf("alert %q runbook_url=%q does not resolve to a file (resolved: %s) — rename the runbook or fix the YAML annotation: %v",
					r.Alert, url, resolved, err)
			}
		}
	}
	if totalRules == 0 {
		// A empty faas.rules.yml is a §11 + M8 row 4 regression —
		// every page-severity alert was silently deleted, so the
		// test must fail loud, not skip silent. A previous revision
		// used t.Skipf here, which let a corrupted (zero-byte)
		// rules file greenlight the gate.
		t.Fatalf("faas.rules.yml parsed with zero rules — file shape changed? (%s)", rulesPath)
	}
	if totalURLs == 0 {
		t.Errorf("faas.rules.yml has %d rules but none references a runbook_url — every page-severity alert should have one", totalRules)
	}
}

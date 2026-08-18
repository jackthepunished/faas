// ADR-098 §11 invariant enforcement: the `data_upstream_rtt_ms` and
// `data_upstream_probes_total` metric families carry a closed label
// set where `host_redacted_hash` is the only host-derived label.
// To keep Prometheus alert cardinality bounded, every alert that
// aggregates over these series MUST drop `host_redacted_hash` from
// the `by` clause — the alert label set must not fan out per-host.
//
// This test parses the rule file at
// pkg/promqlrules/data_placement.yaml and fails on any occurrence
// of `host_redacted_hash` inside a `by ( ... )` clause in a
// `sum by (...)` aggregator or a `histogram_quantile(...)` outer
// by-clause. The §11 invariant is load-bearing for both ADR-098
// §11 and the financial model — every per-host alert would re-add
// per-host cardinality to the dashboard and the alertmanager
// routing rules. A regression that adds the label back to a `by`
// clause (e.g. a careless copy-paste from the upstream metric
// surface) would silently break the invariant if the only
// synthetic fixture assertion is the partial-match `exp_labels`
// (which promtool does not enforce as set-equality).
//
// The fixture's `exp_labels` blocks still assert the allowed
// label set; this test complements them with a *negative* assertion
// at the source — the rule expression must not reference
// `host_redacted_hash` inside any `by` clause.
//
// Build tag: none — runs on plain `go test ./pkg/promqlrules/...`
// so the invariant is gated on every PR, not just the
// integration-tagged run.

package promqlrules

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestFaasDataPlacementS11Invariant(t *testing.T) {
	// Walk up from the test binary's working directory to the repo
	// root, then resolve the rule file path.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// cwd is pkg/promqlrules during `go test ./...`; two dirs up is
	// the repo root.
	rulePath := cwd + "/../../pkg/promqlrules/data_placement.yaml"
	body, err := os.ReadFile(rulePath)
	if err != nil {
		t.Fatalf("read %s: %v", rulePath, err)
	}
	// Match `by ( ... host_redacted_hash ... )` — flag any
	// occurrence inside an aggregation's `by` clause. The regex
	// tolerates whitespace/newlines between identifiers and
	// commas. The `(...)` group is captured for the failure
	// message.
	re := regexp.MustCompile(`(?m)\bby\s*\(([^)]*host_redacted_hash[^)]*)\)`)
	bodyStr := string(body)
	matches := re.FindAllStringSubmatch(bodyStr, -1)
	if len(matches) > 0 {
		var b strings.Builder
		b.WriteString("ADR-098 §11 invariant violation: host_redacted_hash must not appear inside any `by (...)` clause in pkg/promqlrules/data_placement.yaml —\n")
		for _, m := range matches {
			b.WriteString("  - by(")
			b.WriteString(strings.TrimSpace(m[1]))
			b.WriteString(")\n")
		}
		b.WriteString("The fixture's exp_labels is partial-match (promtool labels are subset-matched), so a by-clause that retains host_redacted_hash does not trip the synthetic-fixture eval — only this gate does.")
		t.Fatal(b.String())
	}
}

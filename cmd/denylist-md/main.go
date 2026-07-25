// Command denylist-md generates docs/denylist.md from the shared
// egress catalog (pkg/netns.NewDefaultDenySet + the OCI-only
// client hardening entries in pkg/oci/egress.go). It is the
// operator artifact promised by ADR-034 §Consequences
// (L149-150: "docs/denylist.md lists every deny line with
// source ADR + rationale + test pin").
//
// Wired into the Makefile's `denylist-md` target and run as
// part of `spec-check`, so a stale catalog edit is caught at
// `git diff --exit-code docs/denylist.md` time.
//
// The output is deterministic: v4 entries sort by canonical
// prefix ascending; v6 entries the same; SMTP ports sort
// numerically. Never iterate over a map without sorting first.
//
// Two renderers in one:
//
//   - Platform-wide catalog (NewDefaultDenySet): every entry
//     is enforced by the per-netns forward chain, the host
//     forward chain, AND the OCI dialer. Test pin:
//     pkg/netns/denylist_test.go (provenance + shape) +
//     pkg/netns/denylist_external_test.go (cross-renderer invariant,
//     PR-D).
//   - OCI-only client hardening: the five v4 entries in
//     pkg/oci/egress.go that live ONLY on the OCI dialer (the
//     host firewall does not need them; they are process-level
//     defence-in-depth). Test pin: pkg/oci/egress_test.go.
package main

import (
	"fmt"
	"os"
	"sort"

	"github.com/onebox-faas/faas/pkg/netns"
	"github.com/onebox-faas/faas/pkg/oci"
)

// ociOnlyTestPin is the regression net for every OCI-only entry.
// Hard-coded once; the entry set itself is consumed via
// oci.OCIOnlyDenyCIDRsV4() so the table can never drift from
// the runtime enforcement path (PR-D review).
const ociOnlyTestPin = "pkg/oci/egress_test.go::TestIPAllowed_OCIOnlyEntriesDenied"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "denylist-md:", err)
		os.Exit(1)
	}
}

func run() error {
	ds := netns.NewDefaultDenySet()
	ociOnly := oci.OCIOnlyDenyCIDRsV4()
	out := render(ds, ociOnly)
	if _, err := os.Stdout.WriteString(out); err != nil {
		return fmt.Errorf("write stdout: %w", err)
	}
	return nil
}

// render emits the full markdown document. Pure function; no
// timestamps, no maps iterated without sort, no template strings.
// The OCI-only section is sourced from pkg/oci via the exported
// accessor (PR-D review) so the artifact can never drift from
// the runtime enforcement path.
func render(ds netns.DenySet, ociOnly []netns.DenyEntry) string {
	var b []byte

	// Header.
	b = append(b, []byte("# Tenant egress denylist\n\n")...)
	b = append(b, []byte("<!-- GENERATED — do not edit by hand; regenerate with `make denylist-md`. -->\n\n")...)
	b = append(b, []byte("Single source of truth: [`pkg/netns/denylist.go::NewDefaultDenySet()`](pkg/netns/denylist.go).\n")...)
	b = append(b, []byte("The OCI-only section is the typed `ociOnlyDenyCIDRsV4` array in [`pkg/oci/egress.go`](pkg/oci/egress.go).\n\n")...)

	// Section 1: Platform-wide catalog.
	b = append(b, []byte("## Platform-wide catalog\n\n")...)
	b = append(b, []byte("Enforced by all three sinks: per-netns nftables (table `ip faas` / `ip6 faas`, chain `forward`), host nftables (table `inet faas`, chain `forward`), and the OCI user-space dialer. Cross-renderer invariant pinned by `pkg/netns/denylist_external_test.go::TestAllThreeConsumersAgreeOnDenySet`.\n\n")...)

	// IPv4 rows.
	b = append(b, []byte("### IPv4 CIDRs\n\n")...)
	b = append(b, []byte("| CIDR | Source | Rationale | Test pin |\n")...)
	b = append(b, []byte("|------|--------|-----------|----------|\n")...)
	var v4Rows []denyEntryView
	for _, e := range ds.Entries {
		if e.Family == netns.FamilyV4 {
			v4Rows = append(v4Rows, denyEntryView{Prefix: e.Prefix.String(), SourceADR: e.SourceADR, Comment: e.Comment})
		}
	}
	sort.Slice(v4Rows, func(i, j int) bool { return v4Rows[i].Prefix < v4Rows[j].Prefix })
	for _, r := range v4Rows {
		b = append(b, []byte(fmt.Sprintf("| `%s` | %s | %s | `pkg/netns/denylist_external_test.go::TestAllThreeConsumersAgreeOnDenySet` |\n",
			r.Prefix, r.SourceADR, r.Comment))...)
	}
	b = append(b, '\n')

	// IPv6 rows.
	b = append(b, []byte("### IPv6 CIDRs\n\n")...)
	b = append(b, []byte("| CIDR | Source | Rationale | Test pin |\n")...)
	b = append(b, []byte("|------|--------|-----------|----------|\n")...)
	var v6Rows []denyEntryView
	for _, e := range ds.Entries {
		if e.Family == netns.FamilyV6 {
			v6Rows = append(v6Rows, denyEntryView{Prefix: e.Prefix.String(), SourceADR: e.SourceADR, Comment: e.Comment})
		}
	}
	sort.Slice(v6Rows, func(i, j int) bool { return v6Rows[i].Prefix < v6Rows[j].Prefix })
	for _, r := range v6Rows {
		b = append(b, []byte(fmt.Sprintf("| `%s` | %s | %s | `pkg/netns/denylist_external_test.go::TestAllThreeConsumersAgreeOnDenySet` |\n",
			r.Prefix, r.SourceADR, r.Comment))...)
	}
	b = append(b, '\n')

	// SMTP rows.
	b = append(b, []byte("### SMTP TCP ports\n\n")...)
	b = append(b, []byte("| Port | Source | Rationale | Test pin |\n")...)
	b = append(b, []byte("|------|--------|-----------|----------|\n")...)
	smtpPorts := append([]uint16{}, ds.SMTPPorts...)
	sort.Slice(smtpPorts, func(i, j int) bool { return smtpPorts[i] < smtpPorts[j] })
	for _, p := range smtpPorts {
		b = append(b, []byte(fmt.Sprintf("| `%d` | `spec-§11` | spam = Hetzner abuse desk = existential (spec §7 founding doc R6) | `pkg/netns/denylist_test.go::TestNewDefaultDenySet_SMTPPortsAreComplete`, `pkg/netns/denylist_external_test.go::TestAllThreeConsumersAgreeOnSMTPPorts` |\n",
			p))...)
	}
	b = append(b, '\n')

	// Section 2: OCI-only client hardening.
	b = append(b, []byte("## OCI-only client hardening\n\n")...)
	b = append(b, []byte("These ranges are enforced by the OCI user-space dialer ONLY. They are intentionally NOT in the shared catalog because the host firewall does not need them (no tenant process binds to loopback from the OCI puller, etc.). They are process-level defence-in-depth: if the firewall ever regresses, the user-space check still refuses the dial. Pinned by `pkg/oci/egress_test.go`.\n\n")...)
	b = append(b, []byte("Note: the `0.0.0.0/8` and `127.0.0.0/8` entries are also denied by the `IsLoopback` / `IsUnspecified` predicate in `pkg/oci/egress.go::ipAllowed` (the address-class layer). They are restated here as explicit ranges so the user-space check remains a deny even if the predicate is ever refactored. `192.0.0.0/24`, `198.18.0.0/15`, and `240.0.0.0/4` are NOT covered by the predicate and rely on these entries alone.\n\n")...)

	b = append(b, []byte("### IPv4 CIDRs (OCI-only)\n\n")...)
	b = append(b, []byte("| CIDR | Source | Rationale | Test pin |\n")...)
	b = append(b, []byte("|------|--------|-----------|----------|\n")...)
	// Sort by canonical prefix string so the diff stays reviewable.
	// The entries are typed netns.DenyEntry — SourceADR and Comment
	// are read directly from the accessor, no second source of truth.
	// The IsLoopback/IsUnspecified predicate in pkg/oci/egress.go
	// also denies these ranges at the address-class layer; the
	// entries here are explicit defence-in-depth so the user-space
	// check remains a deny even if the predicate is ever refactored.
	ocr := make([]denyEntryView, 0, len(ociOnly))
	for _, e := range ociOnly {
		ocr = append(ocr, denyEntryView{Prefix: e.Prefix.String(), SourceADR: e.SourceADR, Comment: e.Comment})
	}
	sort.Slice(ocr, func(i, j int) bool { return ocr[i].Prefix < ocr[j].Prefix })
	for _, r := range ocr {
		b = append(b, []byte(fmt.Sprintf("| `%s` | %s | %s | `%s` |\n",
			r.Prefix, r.SourceADR, r.Comment, ociOnlyTestPin))...)
	}

	return string(b)
}

type denyEntryView struct {
	Prefix    string
	SourceADR string
	Comment   string
}
